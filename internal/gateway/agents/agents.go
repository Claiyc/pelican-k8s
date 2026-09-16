// Package agents resolves the agent behind a server, proxies HTTP requests to
// it with the agent token and keeps the short-lived state cache (ARCHITECTURE.md 5.3, 5.8).
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// Target is a resolved agent.
type Target struct {
	UUID    string
	PodIP   string
	TokenID string
	Token   string
}

// HTTPBase returns the agent's HTTP base URL.
func (t Target) HTTPBase() string {
	return "http://" + net.JoinHostPort(t.PodIP, strconv.Itoa(render.AgentPort))
}

// SFTPAddr returns the agent's SFTP address.
func (t Target) SFTPAddr() string { return net.JoinHostPort(t.PodIP, strconv.Itoa(render.SFTPPort)) }

// ErrUnavailable means the pod or agent container is not running.
var ErrUnavailable = errors.New("server pod unavailable")

// Resolver resolves servers to agents.
type Resolver struct {
	Store *store.Store
	// Transport is shared by all proxied requests.
	Transport *http.Transport

	cacheMu sync.Mutex
	cache   map[string]cachedState
	ttl     time.Duration
}

type cachedState struct {
	body []byte
	at   time.Time
}

// NewResolver returns a resolver with a proxy transport tuned for long requests.
func NewResolver(s *store.Store, ttl time.Duration) *Resolver {
	return &Resolver{
		Store: s,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 20 * time.Minute, // compress/decompress can take up to 15 min
			DisableCompression:    true,
		},
		cache: map[string]cachedState{},
		ttl:   ttl,
	}
}

// Resolve returns the agent target of a server, or ErrUnavailable.
func (r *Resolver) Resolve(ctx context.Context, uuid string) (*Target, error) {
	pod, err := r.Store.Pod(ctx, uuid)
	if err != nil {
		return nil, err
	}
	ready, ip := store.PodAgentReady(pod)
	if !ready {
		return nil, ErrUnavailable
	}
	id, token, err := r.Store.AgentToken(ctx, uuid)
	if err != nil {
		return nil, err
	}
	return &Target{UUID: uuid, PodIP: ip, TokenID: id, Token: token}, nil
}

// Proxy forwards the request to the agent with the agent token. The path and
// query are passed unchanged unless rewrite is given.
func (r *Resolver) Proxy(w http.ResponseWriter, req *http.Request, t *Target, rewrite func(*http.Request)) {
	target, _ := url.Parse(t.HTTPBase())
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Authorization", "Bearer "+t.Token)
			pr.Out.Header.Del("X-Forwarded-For")
			if rewrite != nil {
				rewrite(pr.Out)
			}
		},
		Transport:     r.Transport,
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":"agent request failed: %s"}`, jsonEscape(err.Error()))
		},
		ModifyResponse: func(res *http.Response) error {
			// The agent's User-Agent header carries its own token id; the Panel
			// validates ours. The caller sets the node header after we return.
			res.Header.Del("User-Agent")
			return nil
		},
	}
	p.ServeHTTP(w, req)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// Do performs a request against the agent and returns the status and body.
func (r *Resolver) Do(ctx context.Context, t *Target, method, path string, body io.Reader, contentType string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.HTTPBase()+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Transport: r.Transport, Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.StatusCode, b, err
}

// State returns the agent's GET /api/servers/:s body (Wings APIResponse) with
// a short cache. force bypasses the cache.
func (r *Resolver) State(ctx context.Context, t *Target, force bool) ([]byte, error) {
	r.cacheMu.Lock()
	c, ok := r.cache[t.UUID]
	r.cacheMu.Unlock()
	if ok && !force && time.Since(c.at) < r.ttl {
		return c.body, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond) // the Panel waits 1 s
	defer cancel()
	status, body, err := r.Do(ctx, t, http.MethodGet, "/api/servers/"+t.UUID, nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agent returned HTTP %d", status)
	}
	r.cacheMu.Lock()
	r.cache[t.UUID] = cachedState{body: body, at: time.Now()}
	r.cacheMu.Unlock()
	return body, nil
}

// Invalidate drops the cached state of a server.
func (r *Resolver) Invalidate(uuid string) {
	r.cacheMu.Lock()
	delete(r.cache, uuid)
	r.cacheMu.Unlock()
}
