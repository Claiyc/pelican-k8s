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
		caLifetime      = flag.Duration("tls-ca-lifetime", pki.CALifetime, "lifetime of a newly created internal CA")
		caRotation      = flag.Bool("tls-ca-rotation", false, "replace the internal CA automatically once it enters the last third of its lifetime")
		caOverlap       = flag.Duration("tls-ca-rotation-overlap", time.Hour, "how long each step of a CA rotation waits for the new trust bundle to reach every pod")
		tlsDir          = flag.String("tls-dir", "/etc/pelican-tls", "the operator's certificate directory, with --tls-cert-manager-issuer")
		cmIssuer        = flag.String("tls-cert-manager-issuer", "", "issue certificates through this cert-manager issuer instead of the internal CA")
		cmIssuerKind    = flag.String("tls-cert-manager-issuer-kind", "ClusterIssuer", "kind of the cert-manager issuer (Issuer or ClusterIssuer)")
		cmIssuerGroup   = flag.String("tls-cert-manager-issuer-group", "cert-manager.io", "API group of the cert-manager issuer")
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
		// Release the Lease on shutdown so a standby replica takes over at once
		// on a rollout or drain instead of waiting for the Lease to expire. Safe
		// because main exits as soon as the manager returns.
		LeaderElectionReleaseOnCancel: true,
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
		if err := setupTLS(mgr, r, scheme, tlsFlags{
			systemNamespace: *systemNamespace, serversNamespace: *namespace,
			caSecret: *caSecret, gatewaySecret: *gatewaySecret, gatewayNames: splitList(*gatewayNames),
			caLifetime: *caLifetime, rotate: *caRotation, overlap: *caOverlap,
			dir: *tlsDir, issuer: *cmIssuer, issuerKind: *cmIssuerKind, issuerGroup: *cmIssuerGroup,
		}); err != nil {
			logger.Error(err, "unable to set up tls")
			os.Exit(1)
		}
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

type tlsFlags struct {
	systemNamespace, serversNamespace string
	caSecret, gatewaySecret           string
	gatewayNames                      []string
	caLifetime, overlap               time.Duration
	rotate                            bool
	dir                               string
	issuer, issuerKind, issuerGroup   string
}

// setupTLS turns on TLS between the components (ARCHITECTURE.md 12.5, 12.6):
// with a cert-manager issuer the agents get cert-manager Certificates and the
// operator presents its mounted certificate; otherwise the operator keeps the
// internal CA and issues every certificate itself.
func setupTLS(mgr ctrl.Manager, r *controller.GameServerReconciler, scheme *runtime.Scheme, f tlsFlags) error {
	log := ctrl.Log.WithName("certs")
	dial := pki.AgentDialer((&net.Dialer{Timeout: 5 * time.Second}).DialContext)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dial
	r.AgentTransport = transport
	if f.issuer != "" {
		dir, err := pki.LoadDir(f.dir)
		if err != nil {
			return err
		}
		transport.DialTLSContext = pki.TLSDialer(dial, dir.ClientConfig)
		r.CertManager = &certs.CertManager{IssuerName: f.issuer, IssuerKind: f.issuerKind, IssuerGroup: f.issuerGroup}
		log.Info("tls enabled with cert-manager", "issuer", f.issuer, "kind", f.issuerKind)
		return nil
	}
	// Uncached: the manager's cache does not cover the system namespace and
	// is not started yet.
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	ca, trust, err := certs.EnsureCA(ctx, direct, types.NamespacedName{Namespace: f.systemNamespace, Name: f.caSecret}, time.Now(), f.caLifetime)
	cancel()
	if err != nil {
		return err
	}
	issuer, err := pki.NewIssuer(ca, trust, pki.OperatorName)
	if err != nil {
		return err
	}
	transport.DialTLSContext = pki.TLSDialer(dial, issuer.ClientConfig)
	r.PKI = issuer
	if err := mgr.Add(&certs.Manager{
		Client:         direct,
		CAKey:          types.NamespacedName{Namespace: f.systemNamespace, Name: f.caSecret},
		Issuer:         issuer,
		GatewayKey:     types.NamespacedName{Namespace: f.systemNamespace, Name: f.gatewaySecret},
		GatewayNames:   f.gatewayNames,
		AgentNamespace: f.serversNamespace,
		Rotate:         f.rotate,
		Lifetime:       f.caLifetime,
		Overlap:        f.overlap,
		Log:            log,
	}); err != nil {
		return err
	}
	log.Info("tls enabled", "ca", f.caSecret, "gatewaySecret", f.gatewaySecret, "caRotation", f.rotate)
	return nil
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
