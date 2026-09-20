// Package app wires the gateway: Kubernetes cache, Panel client, the
// Panel-facing API, the agent-facing remote API, the websocket proxy, the
// SFTP relay and the drift resync loop.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/metallb"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panelapi"
	"github.com/Claiyc/pelican-k8s/internal/gateway/remoteapi"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/sftprelay"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/gateway/wsproxy"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Gateway holds the wired components.
type Gateway struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Store    *store.Store
	Panel    *panel.Client
	Agents   *agents.Resolver
	Sync     *serversync.Syncer
	PanelAPI *panelapi.Handler
	Remote   *remoteapi.Handler
	Relay    *sftprelay.Relay
	cache    cache.Cache
}

// Scheme returns the runtime scheme used by the gateway.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}

// New builds a gateway from the configuration and a rest config.
func New(ctx context.Context, cfg *config.Config, rc *rest.Config, logger *slog.Logger) (*Gateway, error) {
	scheme := Scheme()
	// Namespaced kinds are cached for the servers namespace only; cluster
	// scoped kinds (classes, nodes) are unaffected by DefaultNamespaces.
	c, err := cache.New(rc, cache.Options{
		Scheme:            scheme,
		DefaultNamespaces: map[string]cache.Config{cfg.ServersNamespace: {}},
	})
	if err != nil {
		return nil, err
	}
	cl, err := client.New(rc, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		return nil, err
	}
	st := store.New(cl, cfg.ServersNamespace, cfg.DefaultClass)
	p := panel.New(cfg.PanelURL, cfg.NodeTokenID, cfg.NodeToken, cfg.UserAgent())
	res := agents.NewResolver(st, cfg.StateCacheTTL)
	sync := &serversync.Syncer{Store: st, Panel: p, Timezone: cfg.Timezone, Log: logger.With("component", "sync")}
	sessions := sftprelay.NewSessions()
	g := &Gateway{Cfg: cfg, Log: logger, Store: st, Panel: p, Agents: res, Sync: sync, cache: c}
	g.PanelAPI = &panelapi.Handler{Cfg: cfg, Store: st, Agents: res, Sync: sync, Log: logger.With("component", "panelapi"), Diagnostics: g.diagnostics}
	if cfg.MetalLBPools {
		// Uncached on purpose: the MetalLB CRD may not be installed, and the
		// cache would start an informer for a kind that does not exist.
		direct, err := client.New(rc, client.Options{Scheme: scheme})
		if err != nil {
			return nil, err
		}
		g.PanelAPI.Pools = &metallb.Pools{
			Reader: direct,
			Names:  cfg.MetalLBPoolNames,
			Max:    cfg.MetalLBMaxAddresses,
			Log:    logger.With("component", "metallb"),
		}
	}
	g.PanelAPI.WS = &wsproxy.Proxy{Cfg: cfg, Store: st, Agents: res, Sync: sync, Log: logger.With("component", "wsproxy"), OriginForAgent: cfg.RemoteURL}
	g.Remote = &remoteapi.Handler{Store: st, Panel: p, Sync: sync, Agents: res, Sftp: sessions, Log: logger.With("component", "remoteapi")}
	g.Relay = &sftprelay.Relay{Listen: cfg.ListenSFTP, Panel: p, Store: st, Agents: res, Sessions: sessions, KeyOnly: cfg.SFTPKeyOnly, Log: logger.With("component", "sftp")}
	return g, nil
}

// Run starts everything and blocks until ctx ends.
func (g *Gateway) Run(ctx context.Context) error {
	errc := make(chan error, 8)
	go func() { errc <- g.cache.Start(ctx) }()
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !g.cache.WaitForCacheSync(wctx) {
		return errors.New("cache sync timed out")
	}
	g.Log.Info("cache synced")

	hostKey, err := g.ensureHostKey(ctx)
	if err != nil {
		return fmt.Errorf("sftp host key: %w", err)
	}
	g.Relay.HostKey = hostKey

	panelSrv := &http.Server{Addr: g.Cfg.ListenPanel, Handler: g.PanelAPI.Routes(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Hour}
	remoteSrv := &http.Server{Addr: g.Cfg.ListenRemote, Handler: g.Remote.Routes(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		g.Log.Info("panel api listening", "addr", g.Cfg.ListenPanel)
		if err := panelSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("panel api: %w", err)
		}
	}()
	go func() {
		g.Log.Info("remote api listening", "addr", g.Cfg.ListenRemote)
		if err := remoteSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("remote api: %w", err)
		}
	}()
	go func() {
		if err := g.Relay.Run(ctx); err != nil {
			errc <- fmt.Errorf("sftp relay: %w", err)
		}
	}()
	go g.Sync.RunResync(ctx, g.Cfg.ResyncInterval)
	go g.resetServersState(ctx)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		if runErr != nil {
			g.Log.Error("component failed", "error", runErr)
		}
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = panelSrv.Shutdown(sctx)
	_ = remoteSrv.Shutdown(sctx)
	return runErr
}

// ensureHostKey loads or creates the gateway SSH host key Secret.
func (g *Gateway) ensureHostKey(ctx context.Context) (sshSigner, error) {
	// The system namespace is not cached: use a direct client.
	direct, err := client.New(clientRestConfig(g), client.Options{Scheme: Scheme()})
	if err != nil {
		return nil, err
	}
	key := types.NamespacedName{Namespace: g.Cfg.SystemNamespace, Name: g.Cfg.SFTPHostKeySecret}
	sec := &corev1.Secret{}
	err = direct.Get(ctx, key, sec)
	if apierrors.IsNotFound(err) {
		pemBytes, err := sftprelay.GenerateHostKey()
		if err != nil {
			return nil, err
		}
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/part-of": "pelican-k8s"}}, Data: map[string][]byte{"id_ed25519": pemBytes}}
		if err := direct.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := direct.Get(ctx, key, sec); err != nil {
			return nil, err
		}
		g.Log.Info("generated sftp host key", "secret", key.String())
	} else if err != nil {
		return nil, err
	}
	return sftprelay.ParseHostKey(sec.Data["id_ed25519"])
}

// resetServersState sends the once-per-boot reset once no install or restore is in flight (section 8.9).
func (g *Gateway) resetServersState(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		list, err := g.Store.List(ctx)
		if err == nil {
			busy := false
			for _, gs := range list {
				installPending := gs.Spec.Install.Generation > gs.Status.Install.ObservedGeneration
				if installPending || gs.Status.Install.Result == v1alpha1.InstallRunning || len(gs.Status.Backups.Pending) > 0 {
					busy = true
					break
				}
			}
			if !busy {
				if err := g.Panel.ResetServersState(ctx); err != nil {
					g.Log.Warn("panel reset failed, retrying", "error", err)
				} else {
					g.Log.Info("panel server states reset")
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *Gateway) diagnostics(ctx context.Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pelican-k8s gateway %s\n", version.Version)
	fmt.Fprintf(&b, "panel: %s\nnode token id: %s\nservers namespace: %s\n", g.Cfg.PanelURL, g.Cfg.NodeTokenID, g.Cfg.ServersNamespace)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := g.Panel.ListServers(pctx); err != nil {
		fmt.Fprintf(&b, "panel reachable: no (%v)\n", err)
	} else {
		fmt.Fprintf(&b, "panel reachable: yes\n")
	}
	list, err := g.Store.List(ctx)
	if err != nil {
		fmt.Fprintf(&b, "gameservers: error %v\n", err)
		return b.String()
	}
	fmt.Fprintf(&b, "gameservers: %d\n", len(list))
	for _, gs := range list {
		ready := "no"
		if _, err := g.Agents.Resolve(ctx, gs.Spec.Panel.UUID); err == nil {
			ready = "yes"
		}
		fmt.Fprintf(&b, "  %s phase=%s process=%s desired=%s agent=%s\n", gs.Spec.Panel.UUID, gs.Status.Phase, gs.Status.Process.State, gs.Spec.Power.Desired, ready)
	}
	return b.String()
}
