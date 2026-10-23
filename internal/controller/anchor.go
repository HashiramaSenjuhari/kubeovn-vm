package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
	"anchorvm.io/anchorvm/internal/kubeovn"
)

// AnchorName is unique per VMPort instance: a VMPort recreated with the same name
// gets a different anchor, so a different OVN port, so a stale tap on a dead host
// can never bind the new port (Kube-OVN ports have no requested-chassis).
func AnchorName(p *v1.VMPort) string {
	return v1.AnchorPrefix + p.Name + "-" + string(p.UID)[:8]
}

// provider of the VMPort's NAD.
func provider(p *v1.VMPort) (prov, nadNS, nadName string) {
	return kubeovn.Provider(p.Spec.Network.NAD, p.Namespace)
}

// buildAnchor renders the never-scheduled pod that makes Kube-OVN own the port.
// ip/mac are pinned values (from status once known, else from spec).
func buildAnchor(p *v1.VMPort, ip, mac, image string) *corev1.Pod {
	prov, nadNS, nadName := provider(p)
	ann := map[string]string{
		kubeovn.NetworksAnnotation:                         nadNS + "/" + nadName,
		kubeovn.Annotation(prov, kubeovn.KeyLogicalSwitch): p.Spec.Network.Subnet,
	}
	if ip != "" {
		ann[kubeovn.Annotation(prov, kubeovn.KeyIPAddress)] = ip
	}
	if mac != "" {
		ann[kubeovn.Annotation(prov, kubeovn.KeyMACAddress)] = mac
	}
	setSecurityAnnotations(ann, prov, p)
	if o := p.Spec.Network.DHCPv4Options; o != "" {
		ann[kubeovn.Annotation(prov, kubeovn.KeyDHCPv4Options)] = o
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        AnchorName(p),
			Namespace:   p.Namespace,
			Annotations: ann,
			Labels: map[string]string{
				v1.AnchorLabel:        "true",
				v1.AnchorPortLabel:    p.Name,
				v1.AnchorPortUIDLabel: string(p.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1.GroupVersion.String(),
				Kind:               "VMPort",
				Name:               p.Name,
				UID:                p.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			// Two independent guards against ever being scheduled: if the pod ran,
			// kube-ovn-cni would plug a veth with the same iface-id as the VM tap.
			SchedulingGates:               []corev1.PodSchedulingGate{{Name: v1.AnchorGate}},
			NodeSelector:                  map[string]string{"anchorvm.io/never-schedule": "true"},
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65535),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "anchor",
				Image: image,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// setSecurityAnnotations writes (or clears) the SG annotations for the port.
func setSecurityAnnotations(ann map[string]string, prov string, p *v1.VMPort) {
	sgKey := kubeovn.Annotation(prov, kubeovn.KeySecurityGroups)
	psKey := kubeovn.Annotation(prov, kubeovn.KeyPortSecurity)
	if len(p.Spec.Network.SecurityGroups) == 0 {
		delete(ann, sgKey)
		delete(ann, psKey)
		return
	}
	ann[sgKey] = strings.Join(p.Spec.Network.SecurityGroups, ",")
	ann[psKey] = "true"
}

// allocation reads what Kube-OVN wrote on the anchor.
func allocation(p *v1.VMPort, pod *corev1.Pod) (ip, mac, lsp string, ok bool) {
	prov, _, _ := provider(p)
	a := pod.Annotations
	if a[kubeovn.Annotation(prov, kubeovn.KeyAllocated)] != "true" {
		return "", "", "", false
	}
	ip = a[kubeovn.Annotation(prov, kubeovn.KeyIPAddress)]
	mac = a[kubeovn.Annotation(prov, kubeovn.KeyMACAddress)]
	return ip, mac, kubeovn.LSPName(pod.Name, pod.Namespace, prov), ip != "" && mac != ""
}
