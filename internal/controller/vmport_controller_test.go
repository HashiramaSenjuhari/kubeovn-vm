package controller

import (
	"context"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
	"anchorvm.io/anchorvm/internal/kubeovn"
)

const prov = "net.ns1.ovn"

func newPort(name, uid string, ip string, created time.Time) *v1.VMPort {
	return &v1.VMPort{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(created), Generation: 1},
		Spec: v1.VMPortSpec{Hypervisor: "hv1", Network: v1.NetworkSpec{NAD: "net", Subnet: "sub", IP: ip}},
	}
}

func setup(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "agent-hv1", Namespace: "sys"},
		Spec: coordv1.LeaseSpec{RenewTime: ptr.To(metav1.NewMicroTime(time.Now()))}}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&v1.VMPort{}).
		WithIndex(&v1.VMPort{}, IndexRequestedIP, RequestedIP).
		WithObjects(append(objs, lease)...).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100),
		SystemNamespace: "sys", AnchorImage: "pause"}, c
}

func reconcile(t *testing.T, r *Reconciler, name string, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: name}}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}
}

func get(t *testing.T, c client.Client, name string) *v1.VMPort {
	t.Helper()
	p := &v1.VMPort{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: name}, p); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return p
}

func anchorOf(t *testing.T, c client.Client, p *v1.VMPort) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: AnchorName(p)}, pod)
	return pod, err
}

// simulate kube-ovn allocating the anchor
func allocate(t *testing.T, c client.Client, pod *corev1.Pod, ip, mac string) {
	t.Helper()
	pod.Annotations[kubeovn.Annotation(prov, "allocated")] = "true"
	pod.Annotations[kubeovn.Annotation(prov, "ip_address")] = ip
	pod.Annotations[kubeovn.Annotation(prov, "mac_address")] = mac
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAllocatePin(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2) // finalizer, then anchor
	p := get(t, c, "web")
	if !contains(p.Finalizers, v1.Finalizer) {
		t.Fatal("finalizer missing")
	}
	pod, err := anchorOf(t, c, p)
	if err != nil {
		t.Fatalf("anchor not created: %v", err)
	}
	if pod.Name != "av-web-11111111" {
		t.Errorf("anchor name %s", pod.Name)
	}
	if len(pod.Spec.SchedulingGates) != 1 || pod.Spec.NodeSelector["anchorvm.io/never-schedule"] != "true" {
		t.Error("anchor must be gated and unschedulable")
	}
	if pod.Annotations[kubeovn.NetworksAnnotation] != "ns1/net" || pod.Annotations[kubeovn.Annotation(prov, "logical_switch")] != "sub" {
		t.Errorf("network annotations %v", pod.Annotations)
	}
	if pod.Labels[v1.AnchorPortUIDLabel] != "11111111-aaaa" || *pod.Spec.TerminationGracePeriodSeconds != 0 {
		t.Error("labels / grace period")
	}

	allocate(t, c, pod, "10.0.0.5", "02:00:00:00:00:05")
	reconcile(t, r, "web", 1)
	p = get(t, c, "web")
	if p.Status.IP != "10.0.0.5" || p.Status.MAC != "02:00:00:00:00:05" ||
		p.Status.LSP != "av-web-11111111.ns1."+prov || p.Status.Phase != v1.PhaseNetworkReady {
		t.Fatalf("status %+v", p.Status)
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1.CondNetworkReady) {
		t.Error("NetworkReady should be true")
	}

	// agent reports plugged → Ready
	p.Status.Agent = &v1.AgentStatus{UID: string(p.UID), Tap: "avx", OVNInstalled: true}
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "web", 1)
	if got := get(t, c, "web").Status.Phase; got != v1.PhaseReady {
		t.Errorf("phase %s, want Ready", got)
	}
}

func TestStaleAgentReportForOtherUIDIsIgnored(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	pod, _ := anchorOf(t, c, p)
	allocate(t, c, pod, "10.0.0.5", "02:00:00:00:00:05")
	p = get(t, c, "web")
	p.Status.Agent = &v1.AgentStatus{UID: "old-uid", Tap: "avx", OVNInstalled: true, CleanupDone: true}
	_ = c.Status().Update(context.Background(), p)
	reconcile(t, r, "web", 1)
	if get(t, c, "web").Status.Phase == v1.PhaseReady {
		t.Error("a report about another VMPort instance must not make this one Ready")
	}
}

func TestDuplicateRequestedIP(t *testing.T) {
	now := time.Now()
	r, c := setup(t, newPort("a", "aaaaaaaa-1", "10.0.0.9", now), newPort("b", "bbbbbbbb-1", "10.0.0.9", now.Add(time.Second)))
	reconcile(t, r, "a", 2)
	reconcile(t, r, "b", 2)
	if _, err := anchorOf(t, c, get(t, c, "a")); err != nil {
		t.Error("oldest requester must get an anchor")
	}
	b := get(t, c, "b")
	if _, err := anchorOf(t, c, b); !apierrors.IsNotFound(err) {
		t.Error("second requester must NOT get an anchor")
	}
	if b.Status.Phase != v1.PhaseIPConflict || !meta.IsStatusConditionTrue(b.Status.Conditions, v1.CondIPConflict) {
		t.Errorf("b status %+v", b.Status)
	}
}

func TestDeleteWaitsForAgentThenFreesAnchor(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	pod, _ := anchorOf(t, c, p)
	allocate(t, c, pod, "10.0.0.5", "02:00:00:00:00:05")
	reconcile(t, r, "web", 1)

	if err := c.Delete(context.Background(), get(t, c, "web")); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "web", 3)
	p = get(t, c, "web")
	if _, err := anchorOf(t, c, p); err != nil {
		t.Fatal("anchor must stay while the agent has not unplugged the NIC (IP would be reused)")
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1.CondDeletionBlocked) {
		t.Error("DeletionBlocked expected")
	}

	p.Status.Agent = &v1.AgentStatus{UID: string(p.UID), CleanupDone: true}
	_ = c.Status().Update(context.Background(), p)
	reconcile(t, r, "web", 1)
	if _, err := anchorOf(t, c, p); !apierrors.IsNotFound(err) {
		t.Fatal("anchor should be deleted after agent cleanup")
	}
	reconcile(t, r, "web", 1)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "web"}, &v1.VMPort{}); !apierrors.IsNotFound(err) {
		t.Error("VMPort should be gone after finalizer removal")
	}
}

func TestForceDelete(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	p.Annotations = map[string]string{v1.ForceDeleteAnnotation: "true"}
	_ = c.Update(context.Background(), p)
	_ = c.Delete(context.Background(), get(t, c, "web"))
	reconcile(t, r, "web", 2)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "web"}, &v1.VMPort{}); !apierrors.IsNotFound(err) {
		t.Error("force-delete should complete without the agent")
	}
}

func TestSecurityGroupSync(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	p.Spec.Network.SecurityGroups = []string{"sg1", "sg2"}
	_ = c.Update(context.Background(), p)
	reconcile(t, r, "web", 1)
	pod, _ := anchorOf(t, c, p)
	if pod.Annotations[kubeovn.Annotation(prov, "security_groups")] != "sg1,sg2" ||
		pod.Annotations[kubeovn.Annotation(prov, "port_security")] != "true" {
		t.Errorf("sg annotations %v", pod.Annotations)
	}
	p = get(t, c, "web")
	p.Spec.Network.SecurityGroups = nil
	_ = c.Update(context.Background(), p)
	reconcile(t, r, "web", 1)
	pod, _ = anchorOf(t, c, p)
	if _, ok := pod.Annotations[kubeovn.Annotation(prov, "security_groups")]; ok {
		t.Error("sg annotation should be removed")
	}
}

func TestForeignPodWithAnchorNameNotAdopted(t *testing.T) {
	p := newPort("web", "11111111-aaaa", "", time.Now())
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: AnchorName(p), Namespace: "ns1",
		Labels: map[string]string{v1.AnchorPortUIDLabel: "someone-else"}}}
	r, c := setup(t, p, foreign)
	reconcile(t, r, "web", 2)
	got := get(t, c, "web")
	if c := meta.FindStatusCondition(got.Status.Conditions, v1.CondNetworkReady); c == nil || c.Reason != "AnchorNameTaken" {
		t.Errorf("want AnchorNameTaken, got %+v", got.Status.Conditions)
	}
}

func TestUntestedKubeOVNRefusesNewPort(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	r.VersionOK = func(context.Context) (bool, string) { return false, "untested" }
	reconcile(t, r, "web", 2)
	if _, err := anchorOf(t, c, get(t, c, "web")); !apierrors.IsNotFound(err) {
		t.Error("no anchor on untested Kube-OVN")
	}
}

func TestRecreatedAnchorKeepsPinnedAddress(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	pod, _ := anchorOf(t, c, p)
	allocate(t, c, pod, "10.0.0.5", "02:00:00:00:00:05")
	reconcile(t, r, "web", 1)
	// an admin bypassed the guard policy and deleted the anchor
	_ = c.Delete(context.Background(), pod)
	reconcile(t, r, "web", 1)
	pod, err := anchorOf(t, c, p)
	if err != nil {
		t.Fatal("anchor should be recreated")
	}
	if pod.Annotations[kubeovn.Annotation(prov, "ip_address")] != "10.0.0.5" ||
		pod.Annotations[kubeovn.Annotation(prov, "mac_address")] != "02:00:00:00:00:05" {
		t.Errorf("recreated anchor must request the pinned IP/MAC: %v", pod.Annotations)
	}
}

func TestAgentUnreachable(t *testing.T) {
	r, c := setup(t, newPort("web", "11111111-aaaa", "", time.Now()))
	r.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	reconcile(t, r, "web", 2)
	p := get(t, c, "web")
	if meta.IsStatusConditionTrue(p.Status.Conditions, v1.CondAgentReachable) {
		t.Error("stale lease must mark agent unreachable")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
