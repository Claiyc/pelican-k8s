// Command operator reconciles GameServer objects into pods, volumes, services
// and install Jobs, and drives the agents. See ARCHITECTURE.md section 7.
package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/certs"
	"github.com/Claiyc/pelican-k8s/internal/operator/controller"
	"github.com/Claiyc/pelican-k8s/internal/operator/imageresolve"
	"github.com/Claiyc/pelican-k8s/internal/pki"
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
		tlsEnabled      = flag.Bool("tls", os.Getenv("PELICAN_TLS") == "true", "secure gateway, operator, agent and shim traffic with the internal CA")
		caSecret        = flag.String("tls-ca-secret", "pelican-ca", "Secret in the system namespace holding the internal CA")
		gatewaySecret   = flag.String("tls-gateway-secret", "pelican-gateway-tls", "Secret in the system namespace the gateway's certificate is written to")
		gatewayNames    = flag.String("tls-gateway-names", "", "comma-separated DNS names of the gateway's remote API (its Service names)")
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
	if *tlsEnabled {
		// Uncached: the manager's cache does not cover the system namespace
		// and is not started yet.
		direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: scheme})
		if err != nil {
			logger.Error(err, "unable to create client")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		ca, err := certs.EnsureCA(ctx, direct, types.NamespacedName{Namespace: *systemNamespace, Name: *caSecret}, time.Now())
		cancel()
		if err != nil {
			logger.Error(err, "unable to load the internal CA")
			os.Exit(1)
		}
		issuer := &pki.Issuer{CA: ca, ClientName: pki.OperatorName}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = issuer.ClientConfig()
		transport.DialContext = pki.AgentDialer((&net.Dialer{Timeout: 5 * time.Second}).DialContext)
		r.PKI, r.AgentTransport = issuer, transport
		if err := mgr.Add(&certs.Gateway{
			Client:   direct,
			Key:      types.NamespacedName{Namespace: *systemNamespace, Name: *gatewaySecret},
			CA:       ca,
			DNSNames: splitList(*gatewayNames),
			Log:      ctrl.Log.WithName("certs"),
		}); err != nil {
			logger.Error(err, "unable to add the gateway certificate runnable")
			os.Exit(1)
		}
		logger.Info("tls enabled", "ca", *caSecret, "gatewaySecret", *gatewaySecret)
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

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
