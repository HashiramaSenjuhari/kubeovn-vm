// anchorvm-agent: runs on a hypervisor and plugs VM taps into OVN for the VMPorts
// whose spec.hypervisor is this host. Never touches VMs or disks.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
	"anchorvm.io/anchorvm/internal/agent"
)

func main() {
	var hv, systemNS, bridge string
	var mtu int
	flag.StringVar(&hv, "hypervisor", "", "this host's name, as used in VMPort spec.hypervisor (required)")
	flag.StringVar(&systemNS, "system-namespace", "anchorvm-system", "namespace of this agent's Lease")
	flag.StringVar(&bridge, "bridge", "br-int", "OVS integration bridge")
	flag.IntVar(&mtu, "mtu", 1400, "tap MTU (match the Kube-OVN subnet MTU)")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if hv == "" {
		fail(fmt.Errorf("--hypervisor is required"))
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			// Only this host's ports are cached (CRD selectable field).
			&v1.VMPort{}: {Field: fields.OneTermEqualSelector("spec.hypervisor", hv)},
		}},
	})
	if err != nil {
		fail(err)
	}
	r := &agent.Reconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Host: agent.Shell{},
		Hypervisor: hv, Bridge: bridge, MTU: mtu,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fail(err)
	}
	if err := mgr.Add(&agent.Heartbeat{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: systemNS, Hypervisor: hv,
		Interval: 10 * time.Second}); err != nil {
		fail(err)
	}
	if err := mgr.Add(&agent.StalePorts{R: r, Interval: 60 * time.Second}); err != nil {
		fail(err)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
