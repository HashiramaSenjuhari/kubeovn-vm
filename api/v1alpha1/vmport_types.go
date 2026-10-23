// Package v1alpha1 contains the VMPort API.
//
// A VMPort is one network card of a libvirt VM that lives on a Kube-OVN subnet.
// anchorvm owns only the network side (IP, MAC, OVN port, tap); the VM and its
// disks belong to whoever created them.
//
// +kubebuilder:object:generate=true
// +groupName=anchorvm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "anchorvm.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// AttachMode says how the NIC reaches the VM.
// +kubebuilder:validation:Enum=TapOnly;HotPlug
type AttachMode string

const (
	// AttachTapOnly: the agent only creates the tap; the VM's own XML references it.
	AttachTapOnly AttachMode = "TapOnly"
	// AttachHotPlug: the agent also hot-plugs exactly this NIC into spec.attach.domain.
	AttachHotPlug AttachMode = "HotPlug"
)

type NetworkSpec struct {
	// NAD is the Kube-OVN NetworkAttachmentDefinition, "namespace/name" or "name"
	// (same namespace as the VMPort).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nad is immutable"
	NAD string `json:"nad"`
	// Subnet is the Kube-OVN Subnet the NIC lives on.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="subnet is immutable"
	Subnet string `json:"subnet"`
	// IP requests a fixed IPv4 address. Empty = Kube-OVN picks one.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ip is immutable"
	IP string `json:"ip,omitempty"`
	// MAC requests a fixed MAC address. Empty = Kube-OVN picks one.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mac is immutable"
	MAC string `json:"mac,omitempty"`
	// SecurityGroups are Kube-OVN SecurityGroup names. Mutable.
	// +optional
	SecurityGroups []string `json:"securityGroups,omitempty"`
	// DHCPv4Options are extra per-port DHCP options, e.g. "mtu=1300". Mutable.
	// +optional
	DHCPv4Options string `json:"dhcpV4Options,omitempty"`
}

type AttachSpec struct {
	// +kubebuilder:default=TapOnly
	// +optional
	Mode AttachMode `json:"mode,omitempty"`
	// Domain is the existing libvirt domain to hot-plug into (HotPlug mode only).
	// +optional
	Domain string `json:"domain,omitempty"`
}

// VMPortSpec is the desired network card.
// +kubebuilder:validation:XValidation:rule="!has(self.attach) || self.attach.mode != 'HotPlug' || (has(self.attach.domain) && self.attach.domain != ”)",message="attach.domain is required for HotPlug"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.attach) || !has(self.attach) || self.attach == oldSelf.attach",message="attach is immutable"
type VMPortSpec struct {
	// Hypervisor is the name the anchorvm-agent on that host runs with.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hypervisor is immutable"
	Hypervisor string      `json:"hypervisor"`
	Network    NetworkSpec `json:"network"`
	// +optional
	Attach AttachSpec `json:"attach,omitempty"`
}

// Phase is a one-word summary for humans; conditions carry the detail.
type Phase string

const (
	PhasePending      Phase = "Pending"      // anchor created, waiting for Kube-OVN
	PhaseNetworkReady Phase = "NetworkReady" // IP/MAC/port allocated, waiting for agent
	PhaseReady        Phase = "Ready"        // tap plugged and OVN reports it installed
	PhaseIPConflict   Phase = "IPConflict"   // requested IP is taken; needs a human
	PhaseUnknown      Phase = "Unknown"      // agent stopped reporting
	PhaseDeleting     Phase = "Deleting"
)

// Condition types.
const (
	CondNetworkReady    = "NetworkReady"
	CondIPConflict      = "IPConflict"
	CondAgentReachable  = "AgentReachable"
	CondDeletionBlocked = "DeletionBlocked"
	CondReady           = "Ready"
	CondTapReady        = "TapReady"
	CondPortBound       = "PortBound"
	CondAttached        = "Attached"
)

// AgentStatus is written only by the agent of spec.hypervisor (enforced by an
// admission policy); everything else in status is written only by the controller.
type AgentStatus struct {
	// UID of the VMPort this report is about (guards against same-name recreation).
	UID string `json:"uid,omitempty"`
	// Tap is the host tap device the VM must use.
	Tap string `json:"tap,omitempty"`
	// OVNInstalled is true when ovn-controller has installed flows for the port.
	OVNInstalled bool `json:"ovnInstalled,omitempty"`
	// AttachedTo is the domain the NIC is hot-plugged into (HotPlug mode).
	AttachedTo string `json:"attachedTo,omitempty"`
	// CleanupDone is set after deletion: NIC detached, OVS port and tap removed.
	CleanupDone bool `json:"cleanupDone,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type VMPortStatus struct {
	Phase Phase  `json:"phase,omitempty"`
	IP    string `json:"ip,omitempty"`
	MAC   string `json:"mac,omitempty"`
	// LSP is the OVN logical switch port name (the tap's iface-id).
	LSP string `json:"lsp,omitempty"`
	// AnchorPod is the never-scheduled pod that holds this port in Kube-OVN.
	AnchorPod          string `json:"anchorPod,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Agent *AgentStatus `json:"agent,omitempty"`
}

// VMPort is one libvirt VM network card on a Kube-OVN subnet.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vmp
// +kubebuilder:selectablefield:JSONPath=`.spec.hypervisor`
// +kubebuilder:printcolumn:name="Hypervisor",type=string,JSONPath=`.spec.hypervisor`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.ip`
// +kubebuilder:printcolumn:name="MAC",type=string,JSONPath=`.status.mac`
// +kubebuilder:printcolumn:name="Tap",type=string,JSONPath=`.status.agent.tap`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="name must be at most 40 characters"
type VMPort struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMPortSpec   `json:"spec"`
	Status VMPortStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type VMPortList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMPort `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMPort{}, &VMPortList{})
}

// Well-known annotations / labels / names shared by controller and agent.
const (
	Finalizer = "anchorvm.io/cleanup"
	// ForceDeleteAnnotation lets an admin finish deletion when the agent is gone.
	ForceDeleteAnnotation = "anchorvm.io/force-delete"
	// RepairAnnotation asks Kube-OVN to re-create a lost OVN port with the pinned IP.
	RepairAnnotation = "anchorvm.io/repair-network"

	AnchorLabel        = "anchorvm.io/anchor"
	AnchorPortLabel    = "anchorvm.io/port"
	AnchorPortUIDLabel = "anchorvm.io/port-uid"
	AnchorGate         = "anchorvm.io/never-schedule"
	AnchorPrefix       = "av-"
)
