package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
)

const (
	tagUID  = "anchorvm-uid"
	tagPort = "anchorvm-port"
	resync  = 15 * time.Second
)

type Reconciler struct {
	Client client.Client
	// APIReader bypasses the cache; used before any destructive decision.
	APIReader  client.Reader
	Host       Host
	Hypervisor string
	Bridge     string
	MTU        int
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.VMPort{}).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	port := &v1.VMPort{}
	if err := r.Client.Get(ctx, req.NamespacedName, port); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if port.Spec.Hypervisor != r.Hypervisor {
		return ctrl.Result{}, nil
	}
	tap := TapName(string(port.UID))
	if !port.DeletionTimestamp.IsZero() {
		return r.cleanup(ctx, port, tap)
	}
	if port.Status.LSP == "" || port.Status.MAC == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil // controller not done yet
	}

	st := v1.AgentStatus{UID: string(port.UID)}
	if old := port.Status.Agent; old != nil && old.UID == st.UID {
		st.Conditions = old.Conditions
	}
	err := r.plug(ctx, port, tap, &st)
	if perr := r.patchAgentStatus(ctx, port, &st); perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil // reported in conditions
	}
	if !st.OVNInstalled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// plug makes sure tap → OVS port (iface-id = LSP) [→ NIC in domain] exist. Idempotent;
// also repairs a hypervisor reboot (taps vanish, OVS rows stay).
func (r *Reconciler) plug(ctx context.Context, port *v1.VMPort, tap string, st *v1.AgentStatus) error {
	set := func(t string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg})
	}
	ok, err := r.Host.TapExists(ctx, tap)
	if err == nil && !ok {
		err = r.Host.CreateTap(ctx, tap, r.MTU)
	}
	if err != nil {
		set(v1.CondTapReady, false, "TapFailed", err.Error())
		return err
	}
	tags := map[string]string{tagUID: string(port.UID), tagPort: port.Namespace + "/" + port.Name}
	if err := r.Host.EnsureOVSPort(ctx, r.Bridge, tap, port.Status.LSP, port.Status.MAC, tags); err != nil {
		set(v1.CondTapReady, false, "OVSPortFailed", err.Error())
		return err
	}
	st.Tap = tap
	set(v1.CondTapReady, true, "Plugged", fmt.Sprintf("tap %s on %s with iface-id %s", tap, r.Bridge, port.Status.LSP))

	if port.Spec.Attach.Mode == v1.AttachHotPlug {
		d := port.Spec.Attach.Domain
		exists, err := r.Host.DomainExists(ctx, d)
		switch {
		case err != nil:
			set(v1.CondAttached, false, "DomainCheckFailed", err.Error())
		case !exists:
			set(v1.CondAttached, false, "DomainNotFound", "domain "+d+" does not exist (yet)")
		default:
			has, err := r.Host.DomainHasTap(ctx, d, tap)
			if err == nil && !has {
				err = r.Host.AttachNIC(ctx, d, NICXML(tap, port.Status.MAC, r.MTU))
				has = err == nil
			}
			if err != nil {
				set(v1.CondAttached, false, "AttachFailed", err.Error())
			} else if has {
				st.AttachedTo = d
				set(v1.CondAttached, true, "Attached", "NIC attached to domain "+d)
			}
		}
	}

	inst, err := r.Host.OVNInstalled(ctx, tap)
	if err != nil {
		set(v1.CondPortBound, false, "CheckFailed", err.Error())
		return err
	}
	st.OVNInstalled = inst
	if inst {
		set(v1.CondPortBound, true, "OVNInstalled", "ovn-controller bound the port on this host")
	} else {
		set(v1.CondPortBound, false, "WaitingForOVN", "ovn-controller has not installed the port yet")
	}
	return nil
}

// cleanup unplugs the NIC (HotPlug), removes OVS port and tap, then reports
// cleanupDone so the controller may free the IP.
func (r *Reconciler) cleanup(ctx context.Context, port *v1.VMPort, tap string) (ctrl.Result, error) {
	if ag := port.Status.Agent; ag != nil && ag.UID == string(port.UID) && ag.CleanupDone {
		return ctrl.Result{}, nil
	}
	st := v1.AgentStatus{UID: string(port.UID), Tap: tap}
	fail := func(reason string, err error) (ctrl.Result, error) {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1.CondTapReady,
			Status: metav1.ConditionUnknown, Reason: reason, Message: err.Error()})
		_ = r.patchAgentStatus(ctx, port, &st)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if port.Spec.Attach.Mode == v1.AttachHotPlug && port.Status.MAC != "" {
		d := port.Spec.Attach.Domain
		exists, err := r.Host.DomainExists(ctx, d)
		if err != nil {
			return fail("DomainCheckFailed", err)
		}
		if exists {
			has, err := r.Host.DomainHasTap(ctx, d, tap)
			if err != nil {
				return fail("DomainCheckFailed", err)
			}
			if has {
				if err := r.Host.DetachNIC(ctx, d, NICXML(tap, port.Status.MAC, r.MTU)); err != nil {
					return fail("DetachFailed", err)
				}
			}
		}
	}
	if err := r.Host.DeleteOVSPort(ctx, r.Bridge, tap); err != nil {
		return fail("OVSPortDeleteFailed", err)
	}
	if err := r.Host.DeleteTap(ctx, tap); err != nil {
		return fail("TapDeleteFailed", err)
	}
	st.CleanupDone = true
	log.FromContext(ctx).Info("NIC unplugged", "port", port.Namespace+"/"+port.Name, "tap", tap)
	return ctrl.Result{}, r.patchAgentStatus(ctx, port, &st)
}

// patchAgentStatus writes only status.agent (the admission policy rejects anything else).
func (r *Reconciler) patchAgentStatus(ctx context.Context, port *v1.VMPort, st *v1.AgentStatus) error {
	if old := port.Status.Agent; old != nil && equalAgent(old, st) {
		return nil
	}
	body, err := json.Marshal(map[string]any{"status": map[string]any{"agent": st}})
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Client.Status().Patch(ctx, port, client.RawPatch(types.MergePatchType, body)))
}

func equalAgent(a, b *v1.AgentStatus) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// StalePorts removes OVS ports/taps this agent created whose VMPort is gone. It is
// deliberately slow: a port is removed only after the API (not the cache) says
// "not found / different UID" on two consecutive passes. Domains and disks are
// never touched.
type StalePorts struct {
	R        *Reconciler
	Interval time.Duration
	suspects map[string]int
}

func (s *StalePorts) Start(ctx context.Context) error {
	s.suspects = map[string]int{}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.pass(ctx)
		}
	}
}

func (s *StalePorts) pass(ctx context.Context) {
	lg := log.FromContext(ctx).WithName("stale-ports")
	owned, err := s.R.Host.OwnedPorts(ctx)
	if err != nil {
		lg.Error(err, "list owned OVS ports")
		return
	}
	seen := map[string]bool{}
	for tap, tags := range owned {
		seen[tap] = true
		ns, name, ok := cut(tags[tagPort])
		if !ok {
			continue
		}
		p := &v1.VMPort{}
		err := s.R.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, p)
		gone := apierrors.IsNotFound(err) || (err == nil && string(p.UID) != tags[tagUID])
		if err != nil && !apierrors.IsNotFound(err) {
			continue // API trouble: never act on uncertainty
		}
		if !gone {
			delete(s.suspects, tap)
			continue
		}
		s.suspects[tap]++
		if s.suspects[tap] < 2 {
			lg.Info("port looks stale, will re-check", "tap", tap, "vmport", tags[tagPort])
			continue
		}
		lg.Info("removing stale port", "tap", tap, "vmport", tags[tagPort])
		if err := s.R.Host.DeleteOVSPort(ctx, s.R.Bridge, tap); err == nil {
			_ = s.R.Host.DeleteTap(ctx, tap)
			delete(s.suspects, tap)
		}
	}
	for tap := range s.suspects {
		if !seen[tap] {
			delete(s.suspects, tap)
		}
	}
}

func cut(nsName string) (string, string, bool) {
	for i := 0; i < len(nsName); i++ {
		if nsName[i] == '/' {
			return nsName[:i], nsName[i+1:], true
		}
	}
	return "", "", false
}

// Heartbeat renews Lease agent-<hypervisor> so the controller knows we are alive.
type Heartbeat struct {
	Client client.Client
	// Reader must be uncached: the agent may only get/update its own Lease by name.
	Reader     client.Reader
	Namespace  string
	Hypervisor string
	Interval   time.Duration
}

func (h *Heartbeat) Start(ctx context.Context) error {
	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		if err := h.renew(ctx); err != nil {
			log.FromContext(ctx).Error(err, "renew lease")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (h *Heartbeat) renew(ctx context.Context) error {
	name := "agent-" + h.Hypervisor
	now := metav1.NewMicroTime(time.Now())
	l := &coordv1.Lease{}
	// The Lease is created at install time: RBAC can restrict update by resourceName
	// but not create, so the agent is only allowed to update its own Lease.
	if err := h.Reader.Get(ctx, types.NamespacedName{Namespace: h.Namespace, Name: name}, l); err != nil {
		return fmt.Errorf("lease %s/%s (create it with the hypervisor install manifest): %w", h.Namespace, name, err)
	}
	l.Spec.LeaseDurationSeconds = ptr.To[int32](40)
	l.Spec.RenewTime = &now
	l.Spec.HolderIdentity = ptr.To(h.Hypervisor)
	return h.Client.Update(ctx, l)
}
