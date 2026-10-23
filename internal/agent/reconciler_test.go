package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
)

type fakeHost struct {
	taps     map[string]bool
	ports    map[string]map[string]string // tap -> external_ids
	inst     map[string]bool
	domains  map[string]map[string]bool // domain -> taps
	failNext error
}

func newFakeHost() *fakeHost {
	return &fakeHost{taps: map[string]bool{}, ports: map[string]map[string]string{}, inst: map[string]bool{},
		domains: map[string]map[string]bool{}}
}
func (f *fakeHost) TapExists(_ context.Context, t string) (bool, error) { return f.taps[t], nil }
func (f *fakeHost) CreateTap(_ context.Context, t string, _ int) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	f.taps[t] = true
	return nil
}
func (f *fakeHost) DeleteTap(_ context.Context, t string) error { delete(f.taps, t); return nil }
func (f *fakeHost) EnsureOVSPort(_ context.Context, _, t, lsp, mac string, tags map[string]string) error {
	ids := map[string]string{"iface-id": lsp, "attached-mac": mac}
	for k, v := range tags {
		ids[k] = v
	}
	f.ports[t] = ids
	return nil
}
func (f *fakeHost) DeleteOVSPort(_ context.Context, _, t string) error {
	delete(f.ports, t)
	return nil
}
func (f *fakeHost) OVNInstalled(_ context.Context, t string) (bool, error) {
	return f.inst[t] && f.ports[t] != nil, nil
}
func (f *fakeHost) OwnedPorts(context.Context) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for t, ids := range f.ports {
		if ids[tagUID] != "" {
			out[t] = ids
		}
	}
	return out, nil
}
func (f *fakeHost) DomainExists(_ context.Context, d string) (bool, error) {
	_, ok := f.domains[d]
	return ok, nil
}
func (f *fakeHost) DomainHasTap(_ context.Context, d, t string) (bool, error) {
	return f.domains[d][t], nil
}
func (f *fakeHost) AttachNIC(_ context.Context, d, xml string) error {
	for t := range f.taps {
		if contains(xml, t) {
			f.domains[d][t] = true
		}
	}
	return nil
}
func (f *fakeHost) DetachNIC(_ context.Context, d, xml string) error {
	for t := range f.domains[d] {
		if contains(xml, t) {
			delete(f.domains[d], t)
		}
	}
	return nil
}

func contains(s, sub string) bool { return sub != "" && strings.Contains(s, sub) }

func readyPort(name, uid, hv string) *v1.VMPort {
	return &v1.VMPort{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", UID: types.UID(uid), Finalizers: []string{v1.Finalizer}},
		Spec:       v1.VMPortSpec{Hypervisor: hv, Network: v1.NetworkSpec{NAD: "net", Subnet: "sub"}},
		Status:     v1.VMPortStatus{IP: "10.0.0.5", MAC: "02:00:00:00:00:05", LSP: "av-" + name + ".ns1.net.ns1.ovn"},
	}
}

func setup(t *testing.T, h *fakeHost, objs ...client.Object) (*Reconciler, client.Client) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1.VMPort{}).WithObjects(objs...).Build()
	return &Reconciler{Client: c, APIReader: c, Host: h, Hypervisor: "hv1", Bridge: "br-int", MTU: 1400}, c
}

func rec(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func agentOf(t *testing.T, c client.Client, name string) *v1.AgentStatus {
	p := &v1.VMPort{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return p.Status.Agent
}

func TestPlugTapOnly(t *testing.T) {
	h := newFakeHost()
	r, c := setup(t, h, readyPort("web", "uid-1", "hv1"))
	rec(t, r, "web")
	tap := TapName("uid-1")
	if !h.taps[tap] || h.ports[tap]["iface-id"] != "av-web.ns1.net.ns1.ovn" || h.ports[tap]["attached-mac"] != "02:00:00:00:00:05" {
		t.Fatalf("tap/port not plugged: %v", h.ports)
	}
	if h.ports[tap][tagUID] != "uid-1" || h.ports[tap][tagPort] != "ns1/web" {
		t.Error("ownership tags missing")
	}
	a := agentOf(t, c, "web")
	if a == nil || a.Tap != tap || a.OVNInstalled || a.UID != "uid-1" {
		t.Fatalf("agent status %+v", a)
	}
	h.inst[tap] = true
	rec(t, r, "web")
	if !agentOf(t, c, "web").OVNInstalled {
		t.Error("ovnInstalled not reported")
	}
}

func TestOtherHypervisorIgnored(t *testing.T) {
	h := newFakeHost()
	r, c := setup(t, h, readyPort("web", "uid-1", "hv2"))
	rec(t, r, "web")
	if len(h.taps) != 0 || agentOf(t, c, "web") != nil {
		t.Error("agent must not touch another hypervisor's port")
	}
}

func TestWaitsForControllerAllocation(t *testing.T) {
	h := newFakeHost()
	p := readyPort("web", "uid-1", "hv1")
	p.Status = v1.VMPortStatus{}
	r, _ := setup(t, h, p)
	rec(t, r, "web")
	if len(h.taps) != 0 {
		t.Error("no tap before the controller pinned LSP/MAC")
	}
}

func TestRebootRecreatesTap(t *testing.T) {
	h := newFakeHost()
	r, _ := setup(t, h, readyPort("web", "uid-1", "hv1"))
	rec(t, r, "web")
	tap := TapName("uid-1")
	delete(h.taps, tap) // host rebooted
	rec(t, r, "web")
	if !h.taps[tap] {
		t.Error("tap should be recreated")
	}
}

func TestHotPlugAttachAndDetach(t *testing.T) {
	h := newFakeHost()
	h.domains["vm1"] = map[string]bool{}
	p := readyPort("web", "uid-1", "hv1")
	p.Spec.Attach = v1.AttachSpec{Mode: v1.AttachHotPlug, Domain: "vm1"}
	r, c := setup(t, h, p)
	rec(t, r, "web")
	tap := TapName("uid-1")
	if !h.domains["vm1"][tap] || agentOf(t, c, "web").AttachedTo != "vm1" {
		t.Fatal("NIC should be hot-plugged into vm1")
	}
	_ = c.Delete(context.Background(), p)
	rec(t, r, "web")
	if h.domains["vm1"][tap] || h.taps[tap] || h.ports[tap] != nil {
		t.Error("cleanup must detach the NIC and remove tap + OVS port")
	}
	if _, ok := h.domains["vm1"]; !ok {
		t.Error("the domain itself must never be removed")
	}
	if !agentOf(t, c, "web").CleanupDone {
		t.Error("cleanupDone not reported")
	}
}

func TestHotPlugDomainMissingIsReportedNotFatal(t *testing.T) {
	h := newFakeHost()
	p := readyPort("web", "uid-1", "hv1")
	p.Spec.Attach = v1.AttachSpec{Mode: v1.AttachHotPlug, Domain: "nope"}
	r, c := setup(t, h, p)
	rec(t, r, "web")
	a := agentOf(t, c, "web")
	if a.AttachedTo != "" || a.Tap == "" {
		t.Errorf("tap should be plugged, attach pending: %+v", a)
	}
}

func TestStalePortsNeedTwoPassesAndAPIConfirmation(t *testing.T) {
	h := newFakeHost()
	r, c := setup(t, h, readyPort("web", "uid-1", "hv1"))
	rec(t, r, "web")
	tap := TapName("uid-1")
	// a leftover port of a VMPort that no longer exists
	h.taps["avstale"] = true
	h.ports["avstale"] = map[string]string{tagUID: "gone", tagPort: "ns1/gone"}
	// a port whose VMPort was recreated with the same name (different UID)
	h.taps["avold"] = true
	h.ports["avold"] = map[string]string{tagUID: "uid-old", tagPort: "ns1/web"}

	s := &StalePorts{R: r, suspects: map[string]int{}}
	s.pass(context.Background())
	if h.ports["avstale"] == nil || h.ports["avold"] == nil {
		t.Fatal("first pass must only suspect, not delete")
	}
	s.pass(context.Background())
	if h.ports["avstale"] != nil || h.ports["avold"] != nil || h.taps["avstale"] {
		t.Error("second pass should remove confirmed stale ports")
	}
	if h.ports[tap] == nil {
		t.Error("live port must never be removed")
	}
	_ = c
}

func TestStalePortsSurviveAPIErrors(t *testing.T) {
	h := newFakeHost()
	r, _ := setup(t, h)
	r.APIReader = errReader{}
	h.ports["avx"] = map[string]string{tagUID: "u", tagPort: "ns1/x"}
	s := &StalePorts{R: r, suspects: map[string]int{}}
	for i := 0; i < 5; i++ {
		s.pass(context.Background())
	}
	if h.ports["avx"] == nil {
		t.Error("API errors must never lead to removal")
	}
}

type errReader struct{ client.Reader }

func (errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return fmt.Errorf("connection refused")
}

func TestTapNameStableAndShort(t *testing.T) {
	a, b := TapName("uid-1"), TapName("uid-1")
	if a != b || len(a) > 15 || TapName("uid-2") == a {
		t.Errorf("tap names %s %s", a, TapName("uid-2"))
	}
	_ = time.Second
}
