// Package agent runs on a hypervisor: it plugs VM taps into OVN for the VMPorts
// assigned to this host. It never creates, starts, stops or deletes VMs or disks;
// in HotPlug mode it attaches/detaches exactly one NIC of an existing domain.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Host is everything the agent does to the machine, behind an interface so the
// reconcile logic is unit-testable.
type Host interface {
	TapExists(ctx context.Context, tap string) (bool, error)
	CreateTap(ctx context.Context, tap string, mtu int) error
	DeleteTap(ctx context.Context, tap string) error
	// EnsureOVSPort adds tap to the integration bridge with iface-id=lsp and our ownership tags.
	EnsureOVSPort(ctx context.Context, bridge, tap, lsp, mac string, tags map[string]string) error
	DeleteOVSPort(ctx context.Context, bridge, tap string) error
	OVNInstalled(ctx context.Context, tap string) (bool, error)
	// OwnedPorts lists OVS interfaces carrying external_ids:anchorvm-uid → tags.
	OwnedPorts(ctx context.Context) (map[string]map[string]string, error)
	DomainExists(ctx context.Context, domain string) (bool, error)
	DomainHasTap(ctx context.Context, domain, tap string) (bool, error)
	AttachNIC(ctx context.Context, domain, xml string) error
	DetachNIC(ctx context.Context, domain, xml string) error
}

// TapName is derived from the VMPort UID (≤15 chars, the Linux limit), so a
// recreated VMPort never reuses a stale tap.
func TapName(uid string) string {
	h := sha256.Sum256([]byte(uid))
	return "av" + hex.EncodeToString(h[:])[:11]
}

// NICXML is the interface element for HotPlug mode (same shape the platform uses
// in TapOnly mode).
func NICXML(tap, mac string, mtu int) string {
	return fmt.Sprintf(`<interface type="ethernet"><mac address="%s"/><target dev="%s" managed="no"/><model type="virtio"/><mtu size="%d"/></interface>`,
		mac, tap, mtu)
}

// Shell implements Host with ip, ovs-vsctl and virsh.
type Shell struct{}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
	}
	return s, nil
}

func (Shell) TapExists(ctx context.Context, tap string) (bool, error) {
	_, err := os.Stat("/sys/class/net/" + tap)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (Shell) CreateTap(ctx context.Context, tap string, mtu int) error {
	if _, err := run(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil &&
		!strings.Contains(err.Error(), "exists") {
		return err
	}
	_, err := run(ctx, "ip", "link", "set", tap, "mtu", fmt.Sprint(mtu), "up")
	return err
}

func (Shell) DeleteTap(ctx context.Context, tap string) error {
	if _, err := os.Stat("/sys/class/net/" + tap); os.IsNotExist(err) {
		return nil
	}
	_, err := run(ctx, "ip", "link", "del", tap)
	return err
}

func (Shell) EnsureOVSPort(ctx context.Context, bridge, tap, lsp, mac string, tags map[string]string) error {
	args := []string{"--may-exist", "add-port", bridge, tap, "--", "set", "interface", tap,
		"external_ids:iface-id=" + lsp, "external_ids:attached-mac=" + mac}
	for k, v := range tags {
		args = append(args, fmt.Sprintf("external_ids:%s=%q", k, v))
	}
	_, err := run(ctx, "ovs-vsctl", args...)
	return err
}

func (Shell) DeleteOVSPort(ctx context.Context, bridge, tap string) error {
	_, err := run(ctx, "ovs-vsctl", "--if-exists", "del-port", bridge, tap)
	return err
}

func (Shell) OVNInstalled(ctx context.Context, tap string) (bool, error) {
	out, err := run(ctx, "ovs-vsctl", "--if-exists", "get", "interface", tap, "external_ids:ovn-installed")
	if err != nil {
		return false, err
	}
	return strings.Trim(out, `"`) == "true", nil
}

func (Shell) OwnedPorts(ctx context.Context) (map[string]map[string]string, error) {
	out, err := run(ctx, "ovs-vsctl", "--format=csv", "--data=bare", "--no-headings",
		"--columns=name,external_ids", "find", "interface", "external_ids:anchorvm-uid!=\"\"")
	if err != nil {
		return nil, err
	}
	res := map[string]map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		name, ids, _ := strings.Cut(line, ",")
		tags := map[string]string{}
		for _, kv := range strings.Fields(strings.Trim(ids, `"`)) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				tags[k] = strings.Trim(v, `"`)
			}
		}
		if tags[tagUID] == "" {
			continue // never treat a port we did not tag as ours
		}
		res[name] = tags
	}
	return res, nil
}

func (Shell) DomainExists(ctx context.Context, domain string) (bool, error) {
	_, err := run(ctx, "virsh", "dominfo", domain)
	if err != nil && strings.Contains(err.Error(), "failed to get domain") {
		return false, nil
	}
	return err == nil, err
}

func (Shell) DomainHasTap(ctx context.Context, domain, tap string) (bool, error) {
	// Check both the live and the persistent definition.
	live, _ := run(ctx, "virsh", "dumpxml", domain)
	cfg, err := run(ctx, "virsh", "dumpxml", "--inactive", domain)
	if err != nil {
		return false, err
	}
	t := fmt.Sprintf(`dev='%s'`, tap)
	return strings.Contains(live, t) || strings.Contains(cfg, t), nil
}

func virshNIC(ctx context.Context, verb, domain, xml string) error {
	f, err := os.CreateTemp("", "anchorvm-nic-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(xml); err != nil {
		return err
	}
	f.Close()
	state, _ := run(ctx, "virsh", "domstate", domain)
	args := []string{verb, domain, f.Name(), "--config"}
	if strings.HasPrefix(state, "running") || strings.HasPrefix(state, "paused") {
		args = append(args, "--live")
	}
	_, err = run(ctx, "virsh", args...)
	return err
}

func (Shell) AttachNIC(ctx context.Context, domain, xml string) error {
	return virshNIC(ctx, "attach-device", domain, xml)
}

func (Shell) DetachNIC(ctx context.Context, domain, xml string) error {
	return virshNIC(ctx, "detach-device", domain, xml)
}
