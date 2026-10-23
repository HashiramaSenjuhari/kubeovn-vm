# anchorvm — how it works (diagrams)

## 1. The problem and the trick

```
 PROBLEM
 ┌────────────────────┐            ┌────────────────────┐
 │  Kube-OVN          │            │  libvirt VM        │
 │  knows only PODS   │     ✗      │  not a pod         │
 │  (IP, port, DHCP,  │ ◄────────► │  Kube-OVN ignores  │
 │   cleanup)         │            │  it or deletes it  │
 └────────────────────┘            └────────────────────┘

 TRICK: give every VM NIC a fake pod that never runs
 ┌────────────────────┐   gives IP/port   ┌──────────────┐
 │  Kube-OVN          │ ────────────────► │ anchor pod   │  (never runs)
 └────────────────────┘                   └──────┬───────┘
                                                 │ VM "wears" its network identity
                                                 ▼
                                          ┌──────────────┐
                                          │ libvirt VM   │  same IP, MAC, port
                                          └──────────────┘
```

**In one line:** Kube-OVN thinks it's managing a pod, but the traffic actually goes to your VM.

---

## 2. Who does what

```
            KUBERNETES (train)                         HYPERVISOR (os-aio)
 ┌─────────────────────────────────────┐      ┌──────────────────────────────┐
 │  VMPort   ← you write this          │      │  anchorvm-agent              │
 │     │                               │      │   • makes tap                │
 │     ▼                               │ ◄──► │   • plugs tap into OVN       │
 │  anchorvm-controller                │ watch│   • (HotPlug) adds NIC to VM │
 │     • makes anchor pod              │report│   • reports "plugged"        │
 │     • copies IP/MAC to VMPort       │      │                              │
 │     • deletes in safe order         │      │  ovn-controller (OVN's own)  │
 │     │                               │      │   • connects tap ↔ OVN port  │
 │     ▼                               │      │                              │
 │  anchor pod (never runs)            │      │  your VM ── tap              │
 │     │                               │      └──────────────▲───────────────┘
 │     ▼                               │                     │
 │  Kube-OVN (stock, unchanged)        │                     │
 │     • IP + MAC (no duplicates)      │                     │
 │     • OVN port + DHCP + SG          │                     │
 │     │                               │                     │
 │     ▼                               │                     │
 │  OVN database ──────────────────────┼──── geneve ─────────┘
 │                                     │
 │  Admission policies (guards)        │
 └─────────────────────────────────────┘
```

| Piece | Job | Why it's needed |
|---|---|---|
| **VMPort** | "I want a NIC for VM X on subnet Y" | the single thing a user creates |
| **Controller** | VMPort → anchor pod → IP in status | Kube-OVN only reacts to pods |
| **Anchor pod** | stand-in that holds the IP and port | makes Kube-OVN own the IP, DHCP, cleanup and restarts |
| **Kube-OVN** | allocates and builds the OVN port | stock and untouched; its allocator prevents duplicate IPs |
| **Agent** | tap + plug into OVN on the hypervisor | the cluster can't reach into the hypervisor itself |
| **ovn-controller** | links tap ↔ OVN port | normal OVN; the tap's `iface-id` is the anchor's port name |
| **Admission policies** | block deleting or editing anchors; limit each agent to its own host | deleting an anchor would free the IP instantly |
| **Lease** | agent heartbeat | lets the controller know whether the hypervisor is alive |

---

## 3. Create flow

```
 you          controller        Kube-OVN          agent             VM
  │ VMPort ─────►│                  │                │                │
  │              │ anchor pod ─────►│                │                │
  │              │◄── IP 10.240.0.20, MAC, port ─────│                │
  │              │ write to VMPort status            │                │
  │              │────────────── status ────────────►│                │
  │              │                  │                │ make tap       │
  │              │                  │◄── tap iface-id = port (OVN binds)
  │              │◄────────── "plugged" ─────────────│                │
  │  Ready ◄─────│                  │                │── tap into VM ►│
  │              │                  │◄──────────── DHCP ──────────────│
  │              │                  │──────── 10.240.0.20 ───────────►│
```

---

## 4. Delete flow (the order is the safety)

```
 kubectl delete vmport
        │
        ▼
 ① agent:      unplug NIC from VM, remove tap      ← VM can't use the IP anymore
        │
        ▼
 ② controller: delete anchor pod
        │
        ▼
 ③ Kube-OVN:   free IP + port                      ← only now can someone else get it
        │
        ▼
     gone

 agent unreachable?  → stops at ① → "DeletionBlocked" → admin sets force-delete
```

---

## 5. Lifecycle

```
 Pending ──► NetworkReady ──► Ready ──► Deleting ──► gone
   │         (IP assigned)    (plugged)
   └──► IPConflict (IP already taken)

 VM reboot / VM deleted by your platform → VMPort stays Ready, IP kept
```

---

## 6. Guards (what can't happen)

```
 ✗ kubectl delete anchor pod          → denied
 ✗ edit / un-gate / evict anchor      → denied
 ✗ two NICs, same IP                  → IPConflict
 ✗ agent os-aio edits another host    → denied
 ✗ agent deletes pods / reads secrets → no permission
```
