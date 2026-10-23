#!/usr/bin/env bash
# Live end-to-end test of anchorvm on train (k8s + Kube-OVN) and os-aio (hypervisor).
# The script plays "the platform": it creates throwaway CirrOS VMs that use the taps
# anchorvm provides. anchorvm itself never creates VMs or disks.
set -uo pipefail
K=${K:-train} HV=${HV:-osvm} HVNAME=${HVNAME:-os-aio}
NS=${NS:-anchorvm-e2e} NAD=${NAD:-shadow-lab/shadow-net} SUBNET=${SUBNET:-shadow-subnet}
PINGER_NS=shadow-lab PINGER=shadow-pod1
BASE=/var/lib/libvirt/images/vp/cirros.img DIR=/var/lib/libvirt/images/anchorvm-e2e
pass=0 fail=0; FAILED=()
ok()  { echo "PASS  $*"; pass=$((pass+1)); }
bad() { echo "FAIL  $*"; fail=$((fail+1)); FAILED+=("$*"); }
check() { local n=$1; shift; if "$@" >/dev/null 2>&1; then ok "$n"; else bad "$n"; fi; }
k()  { ssh "$K" "kubectl $(printf '%q ' "$@")"; }
h()  { ssh "$HV" "$@"; }
st() { k -n "$NS" get vmport "$1" -o jsonpath="{$2}" 2>/dev/null; }
until_() { local n=$1; shift; for _ in $(seq 1 "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 3; done; return 1; }
phase_is() { [ "$(st "$1" .status.phase)" = "$2" ]; }
ping_() { k -n $PINGER_NS exec $PINGER -- ping -c2 -W2 "$1"; }
cond() { k -n "$NS" get vmport "$1" -o jsonpath="{.status.conditions[?(@.type==\"$2\")].reason}"; }
anchor_of() { st "$1" .status.anchorPod; }
lsp_exists() { [ -n "$(ssh "$K" "kubectl -n kube-system exec deploy/ovn-central -c ovn-central -- ovn-nbctl --bare --columns=name find logical_switch_port name=$1")" ]; }
lsp_gone() { ! lsp_exists "$1"; }
ns_taps() { ssh "$HV" "sudo ovs-vsctl --bare --columns=external_ids find interface 'external_ids:anchorvm-uid!=\"\"'" | grep -c "$NS/"; }
tap_on_host() { h "test -e /sys/class/net/$1"; }

vmport() { # name [ip] [mode] [domain] [sg]
  local sg=""; [ -n "${5:-}" ] && sg="securityGroups: [$5]"
  cat <<EOF | k apply -f - >/dev/null
apiVersion: anchorvm.io/v1alpha1
kind: VMPort
metadata: {name: $1, namespace: $NS}
spec:
  hypervisor: $HVNAME
  network: {nad: $NAD, subnet: $SUBNET${2:+, ip: \"$2\"}${sg:+, $sg}}
  attach: {mode: ${3:-TapOnly}${4:+, domain: $4}}
EOF
}

# "platform": define (and optionally start) a CirrOS VM. With a tap it gets the
# anchorvm TapOnly interface; without, it has no NIC (for HotPlug).
platform_vm() { # name [tap mac] [start=yes]
  local n=$1 tap=${2:-} mac=${3:-} start=${4:-yes}
  h "sudo bash -s" <<EOS
set -e; mkdir -p $DIR
printf '{"instance-id": "$n", "local-hostname": "$n"}\n' > $DIR/$n.meta
printf '#!/bin/sh\ntrue\n' > $DIR/$n.user
cloud-localds $DIR/$n.seed.iso $DIR/$n.user $DIR/$n.meta
qemu-img create -q -f qcow2 -F qcow2 -b $BASE $DIR/$n.qcow2
NIC=""; [ -n "$tap" ] && NIC='<interface type="ethernet"><mac address="$mac"/><target dev="$tap" managed="no"/><model type="virtio"/><mtu size="1400"/></interface>'
cat > $DIR/$n.xml <<XML
<domain type="kvm"><name>$n</name><memory unit="MiB">256</memory><vcpu>1</vcpu>
<os><type arch="x86_64">hvm</type><boot dev="hd"/></os><devices>
<disk type="file" device="disk"><driver name="qemu" type="qcow2"/><source file="$DIR/$n.qcow2"/><target dev="vda" bus="virtio"/></disk>
<disk type="file" device="cdrom"><driver name="qemu" type="raw"/><source file="$DIR/$n.seed.iso"/><target dev="sda" bus="sata"/><readonly/></disk>
\$NIC
<serial type="file"><source path="$DIR/$n.console.log"/></serial><console type="file"><source path="$DIR/$n.console.log"/><target type="serial"/></console>
</devices></domain>
XML
virsh define $DIR/$n.xml >/dev/null
[ "$start" = yes ] && virsh start $n >/dev/null || true
EOS
}
platform_vm_delete() { h "sudo virsh destroy $1 >/dev/null 2>&1; sudo virsh undefine $1 >/dev/null 2>&1; sudo rm -f $DIR/$1.*"; }
leased() { h "sudo grep -aq 'leased $2' $DIR/$1.console.log"; }

cleanup() {
  k delete ns "$NS" --wait=false >/dev/null 2>&1
  for v in $(h "sudo virsh list --all --name | grep '^e2e-' || true"); do platform_vm_delete "$v"; done
}
trap cleanup EXIT

echo "== setup"
k create ns "$NS" >/dev/null 2>&1 || true
# request as the agent's own identity (token from its kubeconfig), from the hypervisor
h "sudo tee /usr/local/sbin/anchorvm-e2e-agent-req >/dev/null && sudo chmod 700 /usr/local/sbin/anchorvm-e2e-agent-req" <<'EOS'
#!/bin/sh
# usage: METHOD PATH [merge-patch-json]  -> prints HTTP code
T=$(sed -n 's/.*token: "\(.*\)"}}\].*/\1/p' /etc/anchorvm/kubeconfig)
S=$(sed -n 's/.*server: "\([^"]*\)".*/\1/p' /etc/anchorvm/kubeconfig)
exec curl -sk -o /dev/null -w '%{http_code}' -X "$1" -H "Authorization: Bearer $T" \
  -H "Content-Type: application/merge-patch+json" "$S$2" ${3:+-d "$3"}
EOS
k apply -f - >/dev/null <<EOF
apiVersion: kubeovn.io/v1
kind: SecurityGroup
metadata: {name: anchorvm-e2e-ssh-only}
spec:
  allowSameGroupTraffic: false
  ingressRules: [{ipVersion: ipv4, policy: allow, priority: 10, protocol: tcp, portRangeMin: 22, portRangeMax: 22, remoteType: address, remoteAddress: 0.0.0.0/0}]
  egressRules: [{ipVersion: ipv4, policy: allow, priority: 10, protocol: all, remoteType: address, remoteAddress: 0.0.0.0/0}]
EOF

echo "== E1 TapOnly: VMPort -> IP/MAC/tap -> platform VM uses tap -> DHCP + ping"
vmport p1
check "E1 p1 NetworkReady/Ready" until_ 20 bash -c "[ -n \"\$(ssh $K kubectl -n $NS get vmport p1 -o jsonpath='{.status.agent.tap}')\" ]"
IP1=$(st p1 .status.ip) MAC1=$(st p1 .status.mac) TAP1=$(st p1 .status.agent.tap)
echo "      p1 ip=$IP1 mac=$MAC1 tap=$TAP1 anchor=$(anchor_of p1)"
check "E1 tap exists on hypervisor" tap_on_host "$TAP1"
check "E1 phase Ready (ovn-installed)" until_ 20 phase_is p1 Ready
platform_vm e2e-vm1 "$TAP1" "$MAC1"
check "E1 VM leased anchor IP $IP1" until_ 40 leased e2e-vm1 "$IP1"
check "E1 pod -> VM ping" until_ 20 ping_ "$IP1"

echo "== E2 platform restarts / deletes its VM: VMPort and IP untouched"
h "sudo virsh destroy e2e-vm1 >/dev/null; sudo truncate -s0 $DIR/e2e-vm1.console.log; sudo virsh start e2e-vm1 >/dev/null"
check "E2 re-leased same IP after VM restart" until_ 40 leased e2e-vm1 "$IP1"
check "E2 ping after VM restart" until_ 20 ping_ "$IP1"
platform_vm_delete e2e-vm1
sleep 5
check "E2 VMPort still Ready after platform deleted VM" phase_is p1 Ready
check "E2 IP still reserved (same IP in status)" test "$(st p1 .status.ip)" = "$IP1"

echo "== E3 HotPlug into an existing (stopped) domain, then platform starts it"
platform_vm e2e-vm2 "" "" no
vmport p2 "" HotPlug e2e-vm2
check "E3 p2 Ready (attached)" until_ 30 phase_is p2 Ready
IP2=$(st p2 .status.ip); TAP2=$(st p2 .status.agent.tap)
check "E3 NIC in domain config" bash -c "ssh $HV 'sudo virsh dumpxml --inactive e2e-vm2' | grep -q \"dev='$TAP2'\""
h "sudo virsh start e2e-vm2 >/dev/null"
check "E3 VM leased $IP2" until_ 40 leased e2e-vm2 "$IP2"
check "E3 pod -> VM ping" until_ 20 ping_ "$IP2"

echo "== E4 duplicate / conflicting IPs"
vmport p3 "$IP2"
check "E4 VMPort asking another VMPort's IP -> IPConflict" until_ 15 phase_is p3 IPConflict
PODIP=$(k -n $PINGER_NS get pod $PINGER -o jsonpath='{.metadata.annotations.shadow-net\.shadow-lab\.ovn\.kubernetes\.io/ip_address}')
vmport p4 "$PODIP"
check "E4 VMPort asking a pod's IP ($PODIP) -> IPConflict" until_ 20 phase_is p4 IPConflict
echo "      p4 reason: $(cond p4 IPConflict)"
k -n "$NS" delete vmport p3 p4 --wait=true >/dev/null

echo "== E5 security group on a live VMPort"
k -n "$NS" patch vmport p2 --type merge -p '{"spec":{"network":{"securityGroups":["anchorvm-e2e-ssh-only"]}}}' >/dev/null
sleep 10
if ping_ "$IP2" >/dev/null 2>&1; then bad "E5 ICMP blocked after SG added"; else ok "E5 ICMP blocked after SG added"; fi
check "E5 tcp/22 allowed by SG" until_ 20 bash -c "ssh $K \"kubectl -n $PINGER_NS exec $PINGER -- sh -c 'echo | nc -w 3 $IP2 22'\" | grep -q SSH"
k -n "$NS" patch vmport p2 --type merge -p '{"spec":{"network":{"securityGroups":null}}}' >/dev/null
check "E5 ICMP allowed again after SG removed" until_ 20 ping_ "$IP2"

echo "== E6 anchor pods are protected"
A2=$(anchor_of p2)
denied() { local out; out=$("$@" 2>&1); grep -q "$DENY_MSG" <<<"$out"; }
DENY_MSG="delete its VMPort"
if denied k -n "$NS" delete pod "$A2" --wait=false; then ok "E6 kubectl delete anchor denied"; else bad "E6 kubectl delete anchor denied"; fi
DENY_MSG="managed by anchorvm"
if denied k -n "$NS" patch pod "$A2" --type json -p '[{"op":"remove","path":"/spec/schedulingGates"}]'; then ok "E6 gate removal denied"; else bad "E6 gate removal denied"; fi
if denied k -n "$NS" annotate pod "$A2" foo=bar; then ok "E6 annotation edit denied"; else bad "E6 annotation edit denied"; fi
EV=$(ssh "$K" "kubectl create --raw /api/v1/namespaces/$NS/pods/$A2/eviction -f - <<<'{\"apiVersion\":\"policy/v1\",\"kind\":\"Eviction\",\"metadata\":{\"name\":\"$A2\",\"namespace\":\"$NS\"}}'" 2>&1)
if grep -q "cannot be evicted" <<<"$EV"; then ok "E6 eviction denied"; else bad "E6 eviction denied ($EV)"; fi
check "E6 anchor still there" k -n "$NS" get pod "$A2"

echo "== E7 agent identity is restricted (NodeRestriction-style)"
agent_req() { h "sudo /usr/local/sbin/anchorvm-e2e-agent-req $1 $2 '$3'"; }
agent_patch() { agent_req PATCH "/apis/anchorvm.io/v1alpha1/namespaces/$NS/vmports/$1/status" "$2"; }
check "E7 agent may write status.agent of own port" test "$(agent_patch p1 '{"status":{"agent":{"conditions":[]}}}')" = 200
check "E7 agent may NOT write status.ip" test "$(agent_patch p1 '{"status":{"ip":"10.240.0.250"}}')" = 403
cat <<EOF | k apply -f - >/dev/null
apiVersion: anchorvm.io/v1alpha1
kind: VMPort
metadata: {name: p5, namespace: $NS}
spec: {hypervisor: other-host, network: {nad: $NAD, subnet: $SUBNET}}
EOF
until_ 10 bash -c "[ -n \"\$(ssh $K kubectl -n $NS get vmport p5 -o jsonpath='{.status.ip}')\" ]"
check "E7 agent may NOT write another hypervisor's port" test "$(agent_patch p5 '{"status":{"agent":{"tap":"x"}}}')" = 403
k -n "$NS" annotate vmport p5 anchorvm.io/force-delete=true >/dev/null   # its hypervisor has no agent
k -n "$NS" delete vmport p5 --wait=false >/dev/null
check "E7 agent may NOT delete pods (RBAC)" test "$(agent_req DELETE /api/v1/namespaces/$NS/pods/$A2 '')" = 403

echo "== E8 delete VMPort -> NIC unplugged, anchor + IP freed, IP reusable"
LSP1=$(st p1 .status.lsp)
k -n "$NS" delete vmport p1 --wait=true --timeout=60s >/dev/null
check "E8 tap removed from hypervisor" bash -c "! ssh $HV test -e /sys/class/net/$TAP1"
check "E8 anchor pod gone" bash -c "! ssh $K kubectl -n $NS get pod -l anchorvm.io/port=p1 -o name | grep -q ."
check "E8 OVN port gone" until_ 10 lsp_gone "$LSP1"
vmport p6 "$IP1"
check "E8 freed IP $IP1 reused by new VMPort" until_ 15 bash -c "[ \"\$(ssh $K kubectl -n $NS get vmport p6 -o jsonpath='{.status.ip}')\" = $IP1 ]"

echo "== E9 delete while agent is down -> DeletionBlocked -> force-delete; agent then removes stale tap"
until_ 20 phase_is p6 Ready; TAP6=$(st p6 .status.agent.tap)
h "sudo systemctl stop anchorvm-agent"
k -n "$NS" delete vmport p6 --wait=false >/dev/null
check "E9 DeletionBlocked while agent down" until_ 25 bash -c "[ \"\$(ssh $K \"kubectl -n $NS get vmport p6 -o jsonpath='{.status.conditions[?(@.type==\\\"DeletionBlocked\\\")].status}'\")\" = True ]"
check "E9 anchor kept while blocked (IP not freed)" k -n "$NS" get pod -l anchorvm.io/port=p6 -o name
k -n "$NS" annotate vmport p6 anchorvm.io/force-delete=true >/dev/null
check "E9 force-delete completes" until_ 15 bash -c "! ssh $K kubectl -n $NS get vmport p6"
h "sudo systemctl start anchorvm-agent"
check "E9 tap still present right after agent restart (no hasty removal)" tap_on_host "$TAP6"
check "E9 stale tap removed by agent within ~3 min" until_ 70 bash -c "! ssh $HV test -e /sys/class/net/$TAP6"

echo "== E10 same name recreated -> different anchor and tap"
OLD_ANCHOR=$(anchor_of p2) OLD_TAP=$(st p2 .status.agent.tap)
h "sudo virsh destroy e2e-vm2 >/dev/null 2>&1"
k -n "$NS" delete vmport p2 --wait=true --timeout=60s >/dev/null
vmport p2
until_ 20 phase_is p2 Ready
check "E10 new anchor name" bash -c "[ \"$(anchor_of p2)\" != \"$OLD_ANCHOR\" ] && [ -n \"$(anchor_of p2)\" ]"
check "E10 new tap name" bash -c "[ \"\$(ssh $K kubectl -n $NS get vmport p2 -o jsonpath='{.status.agent.tap}')\" != $OLD_TAP ]"

echo "== E11 agent restart and controller restart change nothing"
IP2=$(st p2 .status.ip)
h "sudo systemctl restart anchorvm-agent"; sleep 20
check "E11 p2 Ready after agent restart" until_ 20 phase_is p2 Ready
check "E11 p2 tap intact" tap_on_host "$(st p2 .status.agent.tap)"
vmport p7
k -n anchorvm-system delete pod -l app=anchorvm-controller --wait=false >/dev/null
check "E11 p7 Ready across controller restart" until_ 40 phase_is p7 Ready
check "E11 exactly one anchor for p7" test "$(k -n "$NS" get pod -l anchorvm.io/port=p7 -o name | wc -l)" = 1
check "E11 p2 IP unchanged" test "$(st p2 .status.ip)" = "$IP2"

echo "== E12 repair: OVN port lost (NB loss) -> repair annotation restores it with same IP"
LSP7=$(st p7 .status.lsp) IP7=$(st p7 .status.ip)
ssh "$K" "kubectl -n kube-system exec deploy/ovn-central -c ovn-central -- ovn-nbctl lsp-del $LSP7"
k -n "$NS" annotate vmport p7 anchorvm.io/repair-network=true >/dev/null
check "E12 OVN port re-created" until_ 20 lsp_exists "$LSP7"
check "E12 same IP after repair" bash -c "[ \"\$(ssh $K kubectl -n kube-system exec deploy/ovn-central -c ovn-central -- ovn-nbctl get logical_switch_port $LSP7 addresses)\" = '[\"$(st p7 .status.mac) $IP7\"]' ]"
check "E12 p7 Ready again" until_ 20 phase_is p7 Ready

echo "== E13 namespace deletion tears everything down in order"
k delete ns "$NS" --wait=true --timeout=120s >/dev/null
check "E13 namespace gone" bash -c "! ssh $K kubectl get ns $NS"
check "E13 no anchorvm taps of the namespace left" test "$(ns_taps)" = 0

echo
echo "RESULT: $pass passed, $fail failed"
for f in "${FAILED[@]}"; do echo "  failed: $f"; done
