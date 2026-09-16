// Command operator reconciles GameServer objects into pods, volumes, services
// and install Jobs, and drives the agents. See ARCHITECTURE.md section 7.
package main

import (
	"flag"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/controller"
	"github.com/Claiyc/pelican-k8s/internal/operator/imageresolve"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

func main() {
	var (
		metricsAddr     = flag.String("metrics-bind-address", ":8443", "metrics endpoint")
		probeAddr       = flag.String("health-probe-bind-address", ":8081", "health probe endpoint")
		leaderElect     = flag.Bool("leader-elect", true, "enable leader election")
		namespace       = flag.String("servers-namespace", envOr("PELICAN_SERVERS_NAMESPACE", "pelican-servers"), "namespace holding GameServers")
		systemNamespace = flag.String("system-namespace", envOr("PELICAN_SYSTEM_NAMESPACE", "pelican-system"), "namespace of the gateway and operator")
		defaultClass    = flag.String("default-class", envOr("PELICAN_DEFAULT_CLASS", "default"), "GameServerClass used when spec.className is empty")
		concurrency     = flag.Int("concurrency", 4, "max concurrent reconciles")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "pelican-operator.pelican-k8s.io",
		Cache: cache.Options{
			// Namespaced objects are watched in the servers namespace only; cluster-scoped
			// kinds (classes, nodes, namespaces) are unaffected.
			DefaultNamespaces: map[string]cache.Config{*namespace: {}},
		},
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}
	r := &controller.GameServerReconciler{
		Client:          mgr.GetClient(),
		Reader:          mgr.GetAPIReader(),
		Recorder:        mgr.GetEventRecorderFor("pelican-operator"),
		SystemNamespace: *systemNamespace,
		Resolver:        imageresolve.NewResolver(authn.DefaultKeychain),
		DefaultClass:    *defaultClass,
	}
	if err := r.SetupWithManager(mgr, *concurrency); err != nil {
		logger.Error(err, "unable to set up controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	logger.Info("starting pelican-operator", "version", version.Version, "namespace", *namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited")
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
