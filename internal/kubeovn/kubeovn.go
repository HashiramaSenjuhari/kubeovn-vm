// Package kubeovn holds the stock Kube-OVN conventions anchorvm relies on.
// Nothing here talks to OVN; Kube-OVN itself does all OVN writes.
package kubeovn

import (
	"fmt"
	"strings"
)

const (
	NetworksAnnotation = "k8s.v1.cni.cncf.io/networks"
	ControllerSA       = "system:serviceaccount:kube-system:ovn"
)

// Provider returns the Kube-OVN provider name of a NAD: "<nad>.<nadNamespace>.ovn".
func Provider(nadRef, defaultNS string) (provider, nadNS, nadName string) {
	nadNS, nadName = defaultNS, nadRef
	if i := strings.IndexByte(nadRef, '/'); i >= 0 {
		nadNS, nadName = nadRef[:i], nadRef[i+1:]
	}
	return fmt.Sprintf("%s.%s.ovn", nadName, nadNS), nadNS, nadName
}

// Annotation returns "<provider>.kubernetes.io/<key>".
func Annotation(provider, key string) string {
	return provider + ".kubernetes.io/" + key
}

// Per-provider annotation keys Kube-OVN reads or writes on a pod.
const (
	KeyLogicalSwitch  = "logical_switch"
	KeyIPAddress      = "ip_address"
	KeyMACAddress     = "mac_address"
	KeyAllocated      = "allocated"
	KeySecurityGroups = "security_groups"
	KeyPortSecurity   = "port_security"
	KeyDHCPv4Options  = "dhcp-v4-options" // hyphens, unlike the other keys
)

// LSPName is the logical switch port Kube-OVN creates for a pod on a provider
// (ovs.PodNameToPortName in kube-ovn: "<pod>.<ns>.<provider>").
func LSPName(pod, namespace, provider string) string {
	return fmt.Sprintf("%s.%s.%s", pod, namespace, provider)
}

// TestedVersions are Kube-OVN images whose undocumented behaviour anchorvm depends
// on has been verified live (allocation of never-scheduled pods, GC keeping them,
// IPAM restore from them, IP release on deletionTimestamp).
var TestedVersions = []string{"v1.16.0"}

// VersionFromImage extracts the tag of an image reference.
func VersionFromImage(image string) string {
	if i := strings.LastIndexByte(image, ':'); i >= 0 && !strings.Contains(image[i:], "/") {
		return image[i+1:]
	}
	return ""
}

func IsTested(version string) bool {
	for _, v := range TestedVersions {
		if v == version {
			return true
		}
	}
	return false
}
