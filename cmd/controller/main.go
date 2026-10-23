// anchorvm-controller: turns VMPorts into anchor pods that stock Kube-OVN manages.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "anchorvm.io/anchorvm/api/v1alpha1"
	"anchorvm.io/anchorvm/internal/controller"
	"anchorvm.io/anchorvm/internal/kubeovn"
)

func main() {
	var (
		systemNS, anchorImage, kubeovnNS, kubeovnDeploy string
		allowUntested                                   bool
	)
	flag.StringVar(&systemNS, "system-namespace", "anchorvm-system", "namespace of agent Leases")
	flag.StringVar(&anchorImage, "anchor-image", "registry.k8s.io/pause:3.10", "image of anchor pods (never pulled: they never run)")
	flag.StringVar(&kubeovnNS, "kubeovn-namespace", "kube-system", "namespace of kube-ovn-controller")
	flag.StringVar(&kubeovnDeploy, "kubeovn-deployment", "kube-ovn-controller", "kube-ovn-controller Deployment name")
	flag.BoolVar(&allowUntested, "allow-untested-kubeovn", false, "create new ports even on an untested Kube-OVN version")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "anchorvm-controller",
		LeaderElectionNamespace: systemNS,
		HealthProbeBindAddress:  ":8081",
		Metrics:                 metricsserver.Options{BindAddress: ":8080"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			// Only anchor pods are cached, not every pod in the cluster.
			&corev1.Pod{}: {Label: labels.SelectorFromSet(labels.Set{v1.AnchorLabel: "true"})},
		}},
	})
	if err != nil {
		fail(err)
	}

	vc := &versionCheck{c: mgr.GetAPIReader(), ns: kubeovnNS, name: kubeovnDeploy, allow: allowUntested}
	r := &controller.Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          scheme,
		Recorder:        mgr.GetEventRecorderFor("anchorvm-controller"),
		APIReader:       mgr.GetAPIReader(),
		SystemNamespace: systemNS,
		AnchorImage:     anchorImage,
		VersionOK:       vc.ok,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fail(err)
	}
	_ = mgr.AddHealthzCheck("ping", healthz.Ping)
	_ = mgr.AddReadyzCheck("ping", healthz.Ping)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err)
	}
}

// versionCheck gates new ports on a Kube-OVN version whose undocumented behaviour
// was verified (see kubeovn.TestedVersions). Cached for 5 minutes.
type versionCheck struct {
	c        client.Reader
	ns, name string
	allow    bool
	mu       sync.Mutex
	at       time.Time
	okv      bool
	msg      string
}

func (v *versionCheck) ok(ctx context.Context) (bool, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.at) < 5*time.Minute {
		return v.okv, v.msg
	}
	d := &appsv1.Deployment{}
	if err := v.c.Get(ctx, types.NamespacedName{Namespace: v.ns, Name: v.name}, d); err != nil {
		v.okv, v.msg = v.allow, fmt.Sprintf("cannot read Kube-OVN version: %v", err)
	} else {
		ver := ""
		if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
			ver = kubeovn.VersionFromImage(cs[0].Image)
		}
		switch {
		case kubeovn.IsTested(ver):
			v.okv, v.msg = true, "Kube-OVN "+ver+" is tested"
		case v.allow:
			v.okv, v.msg = true, "Kube-OVN "+ver+" is untested (allowed by flag)"
		default:
			v.okv, v.msg = false, fmt.Sprintf("Kube-OVN %q is not in the tested list %v; refusing new ports (run with --allow-untested-kubeovn after re-running the e2e suite)", ver, kubeovn.TestedVersions)
		}
	}
	v.at = time.Now()
	return v.okv, v.msg
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
