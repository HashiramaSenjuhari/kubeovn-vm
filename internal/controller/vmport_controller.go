// Package controller reconciles VMPorts into anchor pods that stock Kube-OVN manages.
package controller

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
	"anchorvm.io/anchorvm/internal/kubeovn"
)

const (
	// IndexRequestedIP indexes VMPorts by "<subnet>/<ip>" of spec.network.ip.
	IndexRequestedIP = "anchorvm.io/requested-ip"
	resync           = 20 * time.Second
	// AllocationTimeout: no allocation after this long → look for a conflict event.
	AllocationTimeout = 15 * time.Second
	// LeaseStaleAfter marks an agent unreachable.
	LeaseStaleAfter = 40 * time.Second
)

type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// APIReader reads events directly (filtered server-side) instead of caching
	// every event in the cluster. Falls back to Client when nil (tests).
	APIReader client.Reader
	// Namespace where agent Leases live.
	SystemNamespace string
	// AnchorImage is the (never pulled) container image of anchor pods.
	AnchorImage string
	// VersionOK reports whether Kube-OVN's version is tested (or overridden).
	VersionOK func(ctx context.Context) (bool, string)
	Now       func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RequestedIP is the IndexRequestedIP index function.
func RequestedIP(o client.Object) []string {
	p := o.(*v1.VMPort)
	if p.Spec.Network.IP == "" {
		return nil
	}
	return []string{p.Spec.Network.Subnet + "/" + p.Spec.Network.IP}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.VMPort{}, IndexRequestedIP, RequestedIP); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.VMPort{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=anchorvm.io,resources=vmports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=anchorvm.io,resources=vmports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=anchorvm.io,resources=vmports/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	port := &v1.VMPort{}
	if err := r.Get(ctx, req.NamespacedName, port); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := port.DeepCopy()

	if !port.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, port, orig)
	}

	if !controllerutil.ContainsFinalizer(port, v1.Finalizer) {
		controllerutil.AddFinalizer(port, v1.Finalizer)
		if err := r.Update(ctx, port); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	r.setAgentReachable(ctx, port)

	anchor := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: port.Namespace, Name: AnchorName(port)}, anchor)
	switch {
	case apierrors.IsNotFound(err):
		if res, done := r.createAnchor(ctx, port); done {
			return res, r.patchStatus(ctx, port, orig)
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.patchStatus(ctx, port, orig)
	case err != nil:
		return ctrl.Result{}, err
	}
	if anchor.Labels[v1.AnchorPortUIDLabel] != string(port.UID) {
		// Someone else's pod has our anchor name; never adopt it.
		r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "AnchorNameTaken",
			fmt.Sprintf("pod %s exists but does not belong to this VMPort", anchor.Name))
		port.Status.Phase = v1.PhasePending
		return ctrl.Result{RequeueAfter: resync}, r.patchStatus(ctx, port, orig)
	}
	if !anchor.DeletionTimestamp.IsZero() {
		// Only an admin bypassing the guard policy gets here. Kube-OVN is releasing
		// the IP; wait for the pod to go, then recreate it with the pinned IP/MAC.
		r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "AnchorTerminating",
			"anchor pod is being deleted; it will be recreated with the pinned IP/MAC")
		port.Status.Phase = v1.PhasePending
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.patchStatus(ctx, port, orig)
	}
	port.Status.AnchorPod = anchor.Name

	if err := r.syncMutable(ctx, port, anchor); err != nil {
		return ctrl.Result{}, err
	}
	if port.Annotations[v1.RepairAnnotation] == "true" {
		return r.repair(ctx, port, anchor)
	}

	ip, mac, lsp, ok := allocation(port, anchor)
	if !ok {
		port.Status.Phase = v1.PhasePending
		if r.now().Sub(anchor.CreationTimestamp.Time) > AllocationTimeout {
			if msg := r.conflictEvent(ctx, anchor); msg != "" {
				r.markConflict(port, "KubeOVNAddressConflict", msg)
			} else {
				r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "WaitingForKubeOVN",
					"Kube-OVN has not allocated the anchor yet")
			}
		} else {
			r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "WaitingForKubeOVN",
				"Kube-OVN has not allocated the anchor yet")
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, r.patchStatus(ctx, port, orig)
	}
	if port.Status.IP != "" && (port.Status.IP != ip || port.Status.MAC != mac) {
		// Pinned values must never drift; if they do, something bypassed us.
		lg.Info("allocation drifted from pinned values", "pinnedIP", port.Status.IP, "ip", ip,
			"pinnedMAC", port.Status.MAC, "mac", mac)
		r.Recorder.Eventf(port, corev1.EventTypeWarning, "AllocationDrift",
			"anchor has %s/%s but %s/%s was pinned", ip, mac, port.Status.IP, port.Status.MAC)
	}
	port.Status.IP, port.Status.MAC, port.Status.LSP = ip, mac, lsp
	meta.RemoveStatusCondition(&port.Status.Conditions, v1.CondIPConflict)
	r.setCond(port, v1.CondNetworkReady, metav1.ConditionTrue, "Allocated",
		fmt.Sprintf("Kube-OVN allocated %s / %s", ip, mac))
	if err := r.pinAnchor(ctx, anchor, port, ip, mac); err != nil {
		return ctrl.Result{}, err
	}

	r.computeReady(port)
	return ctrl.Result{RequeueAfter: resync}, r.patchStatus(ctx, port, orig)
}

// createAnchor creates the anchor pod unless something forbids it. done=true means
// the caller should stop this reconcile (status explains why).
func (r *Reconciler) createAnchor(ctx context.Context, port *v1.VMPort) (ctrl.Result, bool) {
	if r.VersionOK != nil && port.Status.IP == "" {
		if ok, msg := r.VersionOK(ctx); !ok {
			r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "UntestedKubeOVN", msg)
			port.Status.Phase = v1.PhasePending
			return ctrl.Result{RequeueAfter: time.Minute}, true
		}
	}
	if loser, winner := r.requestedIPLoser(ctx, port); loser {
		r.markConflict(port, "DuplicateRequest",
			fmt.Sprintf("ip %s is already requested by VMPort %s", port.Spec.Network.IP, winner))
		return ctrl.Result{RequeueAfter: resync}, true
	}
	// Pinned values win over spec: a recreated anchor must come back identical.
	ip, mac := port.Spec.Network.IP, port.Spec.Network.MAC
	if port.Status.IP != "" {
		ip, mac = port.Status.IP, port.Status.MAC
	}
	pod := buildAnchor(port, ip, mac, r.AnchorImage)
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "AnchorCreateFailed", err.Error())
		port.Status.Phase = v1.PhasePending
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true
	}
	r.Recorder.Eventf(port, corev1.EventTypeNormal, "AnchorCreated", "created anchor pod %s", pod.Name)
	port.Status.AnchorPod = pod.Name
	port.Status.Phase = v1.PhasePending
	r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, "WaitingForKubeOVN", "anchor created")
	return ctrl.Result{}, false
}

// requestedIPLoser: of all VMPorts requesting the same subnet/ip, the oldest
// (creationTimestamp, then UID) wins; everyone else must not create an anchor.
func (r *Reconciler) requestedIPLoser(ctx context.Context, port *v1.VMPort) (bool, string) {
	if port.Spec.Network.IP == "" {
		return false, ""
	}
	list := &v1.VMPortList{}
	if err := r.List(ctx, list, client.MatchingFields{
		IndexRequestedIP: port.Spec.Network.Subnet + "/" + port.Spec.Network.IP}); err != nil {
		return false, ""
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return items[i].UID < items[j].UID
	})
	if len(items) > 0 && items[0].UID != port.UID {
		return true, items[0].Namespace + "/" + items[0].Name
	}
	return false, ""
}

// conflictEvent returns Kube-OVN's AddressConflict message for the anchor, if any.
func (r *Reconciler) conflictEvent(ctx context.Context, anchor *corev1.Pod) string {
	evs := &corev1.EventList{}
	var rd client.Reader = r.Client
	opts := []client.ListOption{client.InNamespace(anchor.Namespace)}
	if r.APIReader != nil {
		rd = r.APIReader
		opts = append(opts, client.MatchingFields{"involvedObject.uid": string(anchor.UID)})
	}
	if err := rd.List(ctx, evs, opts...); err != nil {
		return ""
	}
	for _, e := range evs.Items {
		if e.InvolvedObject.UID == anchor.UID && e.Reason == "AcquireAddressFailed" {
			return e.Message
		}
	}
	return ""
}

func (r *Reconciler) markConflict(port *v1.VMPort, reason, msg string) {
	port.Status.Phase = v1.PhaseIPConflict
	r.setCond(port, v1.CondIPConflict, metav1.ConditionTrue, reason, msg)
	r.setCond(port, v1.CondNetworkReady, metav1.ConditionFalse, reason, msg)
	r.setCond(port, v1.CondReady, metav1.ConditionFalse, reason, msg)
}

// pinAnchor writes the allocated IP/MAC back onto the anchor so a recreated anchor
// (or a Kube-OVN re-allocation) requests exactly the same address.
func (r *Reconciler) pinAnchor(ctx context.Context, anchor *corev1.Pod, port *v1.VMPort, ip, mac string) error {
	prov, _, _ := provider(port)
	ipKey, macKey := kubeovn.Annotation(prov, kubeovn.KeyIPAddress), kubeovn.Annotation(prov, kubeovn.KeyMACAddress)
	if anchor.Annotations[ipKey] == ip && anchor.Annotations[macKey] == mac {
		return nil
	}
	base := anchor.DeepCopy()
	anchor.Annotations[ipKey], anchor.Annotations[macKey] = ip, mac
	return r.Patch(ctx, anchor, client.MergeFrom(base))
}

// syncMutable propagates mutable spec fields (security groups, DHCP options).
func (r *Reconciler) syncMutable(ctx context.Context, port *v1.VMPort, anchor *corev1.Pod) error {
	prov, _, _ := provider(port)
	want := maps.Clone(anchor.Annotations)
	setSecurityAnnotations(want, prov, port)
	dk := kubeovn.Annotation(prov, kubeovn.KeyDHCPv4Options)
	if o := port.Spec.Network.DHCPv4Options; o != "" {
		want[dk] = o
	} else {
		delete(want, dk)
	}
	if maps.Equal(want, anchor.Annotations) {
		return nil
	}
	base := anchor.DeepCopy()
	anchor.Annotations = want
	if err := r.Patch(ctx, anchor, client.MergeFrom(base)); err != nil {
		return err
	}
	r.Recorder.Event(port, corev1.EventTypeNormal, "AnchorUpdated", "security groups / DHCP options synced")
	return nil
}

// repair drops Kube-OVN's "allocated" mark so it re-creates the OVN port with the
// pinned IP/MAC (used after an OVN NB loss or restore).
func (r *Reconciler) repair(ctx context.Context, port *v1.VMPort, anchor *corev1.Pod) (ctrl.Result, error) {
	prov, _, _ := provider(port)
	if port.Status.IP == "" {
		return ctrl.Result{}, fmt.Errorf("repair requested before any IP was pinned")
	}
	base := anchor.DeepCopy()
	delete(anchor.Annotations, kubeovn.Annotation(prov, kubeovn.KeyAllocated))
	anchor.Annotations[kubeovn.Annotation(prov, kubeovn.KeyIPAddress)] = port.Status.IP
	anchor.Annotations[kubeovn.Annotation(prov, kubeovn.KeyMACAddress)] = port.Status.MAC
	if err := r.Patch(ctx, anchor, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	pbase := port.DeepCopy()
	delete(port.Annotations, v1.RepairAnnotation)
	if err := r.Patch(ctx, port, client.MergeFrom(pbase)); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(port, corev1.EventTypeNormal, "RepairRequested", "asked Kube-OVN to re-create the OVN port")
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, port, orig *v1.VMPort) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(port, v1.Finalizer) {
		return ctrl.Result{}, nil
	}
	port.Status.Phase = v1.PhaseDeleting
	r.setAgentReachable(ctx, port)
	ag := port.Status.Agent
	agentDone := ag != nil && ag.UID == string(port.UID) && ag.CleanupDone
	forced := port.Annotations[v1.ForceDeleteAnnotation] == "true"
	if !agentDone && !forced {
		// Never time out on our own: freeing the IP while the NIC may still exist
		// would hand the address to someone else.
		reason, msg := "WaitingForAgent", "waiting for agent "+port.Spec.Hypervisor+" to unplug the NIC"
		if c := meta.FindStatusCondition(port.Status.Conditions, v1.CondAgentReachable); c != nil && c.Status == metav1.ConditionFalse {
			reason = "AgentUnreachable"
			msg += "; agent is unreachable (set annotation " + v1.ForceDeleteAnnotation + "=true to force)"
		}
		r.setCond(port, v1.CondDeletionBlocked, metav1.ConditionTrue, reason, msg)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.patchStatus(ctx, port, orig)
	}

	// Find the anchor by name and label, never by ownerRef (it may have been orphaned).
	anchor := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: port.Namespace, Name: AnchorName(port)}, anchor)
	if err == nil && anchor.Labels[v1.AnchorPortUIDLabel] == string(port.UID) {
		if anchor.DeletionTimestamp.IsZero() {
			uid := anchor.UID
			if err := r.Delete(ctx, anchor, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(port, corev1.EventTypeNormal, "AnchorDeleted",
				"NIC unplugged (forced=%v); deleted anchor %s, Kube-OVN frees %s", forced, anchor.Name, port.Status.IP)
		}
		return ctrl.Result{RequeueAfter: time.Second}, r.patchStatus(ctx, port, orig)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(port, v1.Finalizer)
	return ctrl.Result{}, r.Update(ctx, port)
}

// setAgentReachable reflects the hypervisor agent's Lease.
func (r *Reconciler) setAgentReachable(ctx context.Context, port *v1.VMPort) {
	lease := &coordv1.Lease{}
	err := r.Get(ctx, types.NamespacedName{Namespace: r.SystemNamespace, Name: "agent-" + port.Spec.Hypervisor}, lease)
	if err != nil || lease.Spec.RenewTime == nil {
		r.setCond(port, v1.CondAgentReachable, metav1.ConditionFalse, "NoLease",
			"no heartbeat from agent "+port.Spec.Hypervisor)
		return
	}
	age := r.now().Sub(lease.Spec.RenewTime.Time)
	if age > LeaseStaleAfter {
		r.setCond(port, v1.CondAgentReachable, metav1.ConditionFalse, "LeaseStale",
			fmt.Sprintf("last agent heartbeat %s ago", age.Round(time.Second)))
		return
	}
	r.setCond(port, v1.CondAgentReachable, metav1.ConditionTrue, "Heartbeat", "agent is reporting")
}

func (r *Reconciler) computeReady(port *v1.VMPort) {
	ag := port.Status.Agent
	agentOK := ag != nil && ag.UID == string(port.UID) && ag.OVNInstalled && ag.Tap != ""
	if agentOK && port.Spec.Attach.Mode == v1.AttachHotPlug {
		agentOK = ag.AttachedTo == port.Spec.Attach.Domain
	}
	reach := meta.FindStatusCondition(port.Status.Conditions, v1.CondAgentReachable)
	switch {
	case reach != nil && reach.Status == metav1.ConditionFalse:
		port.Status.Phase = v1.PhaseUnknown
		r.setCond(port, v1.CondReady, metav1.ConditionUnknown, "AgentUnreachable", reach.Message)
	case agentOK:
		port.Status.Phase = v1.PhaseReady
		r.setCond(port, v1.CondReady, metav1.ConditionTrue, "Plugged", "tap plugged and installed by OVN")
	default:
		port.Status.Phase = v1.PhaseNetworkReady
		r.setCond(port, v1.CondReady, metav1.ConditionFalse, "WaitingForAgent", "waiting for agent to plug the tap")
	}
}

func (r *Reconciler) setCond(port *v1.VMPort, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: port.Generation,
	})
}

// patchStatus sends only what the controller changed; status.agent is never part
// of the diff because the controller never modifies it.
func (r *Reconciler) patchStatus(ctx context.Context, port, orig *v1.VMPort) error {
	port.Status.ObservedGeneration = port.Generation
	if port.Status.Phase != v1.PhaseDeleting {
		meta.RemoveStatusCondition(&port.Status.Conditions, v1.CondDeletionBlocked)
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, port, client.MergeFrom(orig)))
}
