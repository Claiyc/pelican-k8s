// Package wsproxy relays console websockets between browsers and agents,
// re-signing auth tokens and turning power intents into spec changes
// (ARCHITECTURE.md 5.5, 5.9, 8.3).
package wsproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

// Message is the websocket frame format.
type Message struct {
	Event string   `json:"event"`
	Args  []string `json:"args"`
}

// Proxy relays websocket sessions.
type Proxy struct {
	Cfg    *config.Config
	Store  *store.Store
	Agents *agents.Resolver
	Sync   *serversync.Syncer
	Log    *slog.Logger
	// OriginForAgent is sent as Origin when dialing agents (their Panel URL).
	OriginForAgent string

	// netDial overrides how agents are dialed (tests; the agent port is fixed).
	netDial func(ctx context.Context, network, addr string) (net.Conn, error)
	// pollEvery and grace override how often and how long a session looks
	// for a replaced agent pod (tests).
	pollEvery, grace time.Duration
}

var powerPermissions = map[string]string{
	"start":   "control.start",
	"stop":    "control.stop",
	"restart": "control.restart",
	"kill":    "control.stop",
}

func (p *Proxy) checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == p.Cfg.PanelURL {
		return true
	}
	for _, allowed := range p.Cfg.AllowedOrigins {
		if allowed == "*" || allowed == o {
			return true
		}
	}
	return false
}

// ServeHTTP handles GET /api/servers/{server}/ws.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("server")
	gs, err := p.Store.Get(r.Context(), uuid)
	if err != nil {
		http.Error(w, `{"error":"The requested resource does not exist on this instance."}`, http.StatusNotFound)
		return
	}
	upgrader := websocket.Upgrader{EnableCompression: true, CheckOrigin: p.checkOrigin}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.Log.Warn("websocket upgrade rejected", "uuid", uuid, "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr, "error", err)
		return
	}
	defer client.Close()
	p.Log.Info("websocket session opened", "uuid", uuid, "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
	defer p.Log.Info("websocket session closed", "uuid", uuid, "remote", r.RemoteAddr)
	client.SetReadLimit(4096)

	if st, err := settings.Parse(gs.Spec.Panel.Settings); err == nil && st.Suspended {
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4409, "server is suspended"), time.Now().Add(time.Second))
		return
	}

	t, err := p.Agents.Wait(r.Context(), uuid)
	if err != nil {
		_ = client.WriteJSON(Message{Event: "daemon error", Args: []string{"server pod unavailable"}})
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "server pod unavailable"), time.Now().Add(time.Second))
		return
	}
	agent, err := p.dial(uuid, t)
	if err != nil {
		p.Log.Warn("agent websocket dial failed", "uuid", uuid, "error", err)
		_ = client.WriteJSON(Message{Event: "daemon error", Args: []string{"could not reach the server agent"}})
		return
	}

	session := &session{p: p, uuid: uuid, client: client}
	session.attach(agent, t)
	session.run(r.Context())
}

// dial opens the agent side of a session.
func (p *Proxy) dial(uuid string, t *agents.Target) (*websocket.Conn, error) {
	header := http.Header{}
	if p.OriginForAgent != "" {
		header.Set("Origin", p.OriginForAgent)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: true, NetDialContext: p.netDial}
	if c := p.Agents.TLSConfig(); c != nil {
		dialer.TLSClientConfig = c
		dial := p.netDial
		if dial == nil {
			dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		}
		dialer.NetDialContext = pki.AgentDialer(dial)
	}
	agent, resp, err := dialer.Dial(t.WSBase()+"/api/servers/"+uuid+"/ws", header)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return agent, err
}

type session struct {
	p      *Proxy
	uuid   string
	client *websocket.Conn

	mu         sync.Mutex
	claims     *jwtx.Claims
	agentToken string
	agentPod   string

	clientWriteMu sync.Mutex
	// agentWriteMu guards agent, which is nil while the agent pod is replaced.
	agentWriteMu sync.Mutex
	agent        *websocket.Conn
}

func (s *session) writeClient(v any) error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	return s.client.WriteJSON(v)
}

func (s *session) closeClient(code int, text string) {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	_ = s.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
}

// writeAgentRaw forwards a frame to the agent. Without an agent (it is being
// replaced) the frame is dropped; a failed write is noticed by the agent pump.
func (s *session) writeAgentRaw(mt int, data []byte) {
	s.agentWriteMu.Lock()
	defer s.agentWriteMu.Unlock()
	if s.agent != nil {
		_ = s.agent.WriteMessage(mt, data)
	}
}

// attach makes conn, to the agent t, the agent side of the session.
func (s *session) attach(conn *websocket.Conn, t *agents.Target) {
	s.mu.Lock()
	s.agentToken, s.agentPod = t.Token, t.PodUID
	s.mu.Unlock()
	s.agentWriteMu.Lock()
	s.agent = conn
	s.agentWriteMu.Unlock()
}

// detach closes the agent side, if any.
func (s *session) detach() {
	s.agentWriteMu.Lock()
	defer s.agentWriteMu.Unlock()
	if s.agent != nil {
		_ = s.agent.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = s.agent.Close()
		s.agent = nil
	}
}

func (s *session) agentConn() *websocket.Conn {
	s.agentWriteMu.Lock()
	defer s.agentWriteMu.Unlock()
	return s.agent
}

// run relays until the browser leaves or the agent side ends for good. When
// the agent side ends because the agent pod is being replaced, the browser's
// connection is held and the session continues with the new agent
// (ARCHITECTURE.md 5.9).
func (s *session) run(ctx context.Context) {
	clientDone := make(chan struct{})
	go func() {
		s.pumpClientToAgent(ctx)
		close(clientDone)
		s.detach()
	}()
	defer s.detach()
	for {
		clientGone, agentErr := s.pumpAgentToClient(s.agentConn())
		if clientGone || closed(clientDone) {
			return
		}
		if !s.agentReplaced(ctx, clientDone) {
			var ce *websocket.CloseError
			if errors.As(agentErr, &ce) {
				s.closeClient(ce.Code, ce.Text)
			}
			return
		}
		s.p.Log.Info("agent pod is being replaced; holding the console", "uuid", s.uuid)
		s.detach()
		if !s.reattach(ctx, clientDone) {
			return
		}
		s.p.Log.Info("console moved to the new agent pod", "uuid", s.uuid)
	}
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// agentReplaced reports whether the agent pod the session was talking to is
// going or gone. The agent closes its websockets when it is told to stop,
// which can reach the gateway before the pod's deletion does, so a healthy
// looking pod is checked again for a moment.
func (s *session) agentReplaced(ctx context.Context, clientDone <-chan struct{}) bool {
	s.mu.Lock()
	uid := s.agentPod
	s.mu.Unlock()
	grace := time.NewTimer(s.p.replaceGrace())
	defer grace.Stop()
	tick := time.NewTicker(s.p.poll())
	defer tick.Stop()
	for {
		// A deleted server's agent does not come back.
		if gs, err := s.p.Store.Get(ctx, s.uuid); apierrors.IsNotFound(err) || err == nil && !gs.DeletionTimestamp.IsZero() {
			return false
		}
		pod, err := s.p.Store.AgentPod(ctx, s.uuid)
		if err == nil {
			if pod == nil || !pod.DeletionTimestamp.IsZero() || string(pod.UID) != uid {
				return true
			}
			if ready, _ := store.PodAgentReady(pod); !ready {
				return true
			}
		}
		select {
		case <-grace.C:
			return false
		case <-clientDone:
			return false
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// reattach waits up to the configured agentWait for the new agent and
// dials it. The browser is then asked for a fresh token ("token expiring"),
// which the new agent needs: its boot cutoff rejects tokens issued before it
// started. When the wait runs out the browser gets "daemon error" and 1013.
func (s *session) reattach(ctx context.Context, clientDone <-chan struct{}) bool {
	deadline := time.NewTimer(s.p.agentWait())
	defer deadline.Stop()
	tick := time.NewTicker(s.p.poll())
	defer tick.Stop()
	for {
		select {
		case <-clientDone:
			return false
		case <-ctx.Done():
			return false
		case <-deadline.C:
			s.p.Log.Warn("agent pod did not come back; closing the console", "uuid", s.uuid)
			_ = s.writeClient(Message{Event: "daemon error", Args: []string{"the server agent is unavailable"}})
			s.closeClient(websocket.CloseTryAgainLater, "server agent unavailable")
			return false
		case <-tick.C:
			t, err := s.p.Agents.Resolve(ctx, s.uuid)
			if err != nil {
				continue
			}
			conn, err := s.p.dial(s.uuid, t)
			if err != nil {
				continue
			}
			s.attach(conn, t)
			_ = s.writeClient(Message{Event: "token expiring"})
			return true
		}
	}
}

func (p *Proxy) agentWait() time.Duration {
	if p.Cfg.AgentWait > 0 {
		return p.Cfg.AgentWait
	}
	return 2 * time.Minute
}

func (p *Proxy) poll() time.Duration {
	if p.pollEvery > 0 {
		return p.pollEvery
	}
	return time.Second
}

func (p *Proxy) replaceGrace() time.Duration {
	if p.grace > 0 {
		return p.grace
	}
	return 3 * time.Second
}

// pumpAgentToClient passes agent frames through to the browser until either
// side closes. It reports whether the browser went away, and otherwise
// returns the agent's read error.
func (s *session) pumpAgentToClient(agent *websocket.Conn) (clientGone bool, agentErr error) {
	if agent == nil {
		return false, nil
	}
	for {
		mt, data, err := agent.ReadMessage()
		if err != nil {
			return false, err
		}
		if !s.writeClientRaw(mt, data) {
			return true, nil
		}
	}
}

// writeClientRaw passes a frame to the browser and reports whether it went.
func (s *session) writeClientRaw(mt int, data []byte) bool {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	return s.client.WriteMessage(mt, data) == nil
}

// pumpClientToAgent forwards browser frames to the agent, inspecting auth and
// set state, until the browser closes. While the agent pod is replaced the
// frames are dropped, except set state, which the gateway handles itself.
func (s *session) pumpClientToAgent(ctx context.Context) {
	for {
		mt, data, err := s.client.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			s.writeAgentRaw(mt, data)
			continue
		}
		switch m.Event {
		case "auth":
			s.handleAuth(m)
		case "set state":
			s.setState(ctx, strings.Join(m.Args, ""))
		default:
			s.writeAgentRaw(mt, data)
		}
	}
}

// handleAuth verifies the Panel-signed JWT of an auth frame, re-signs it with
// the agent token, remembers its claims for later permission checks and
// forwards it to the agent. A rejected token is reported to the browser as a
// "jwt error".
func (s *session) handleAuth(m Message) {
	claims, resigned, rejection := s.verifyAuth(m)
	if rejection != "" {
		_ = s.writeClient(Message{Event: "jwt error", Args: []string{rejection}})
		return
	}
	s.mu.Lock()
	s.claims = claims
	s.mu.Unlock()
	out, _ := json.Marshal(Message{Event: "auth", Args: []string{string(resigned)}})
	s.writeAgentRaw(websocket.TextMessage, out)
}

// verifyAuth checks the token of an auth frame against the node token and this
// session's server, and re-signs it with the agent token. A non-empty
// rejection is the text to report to the browser.
func (s *session) verifyAuth(m Message) (claims *jwtx.Claims, resigned []byte, rejection string) {
	claims, raw, err := jwtx.Verify([]byte(strings.Join(m.Args, "")), []byte(s.p.Cfg.NodeToken))
	if err != nil {
		// Verify's errors already start with "jwt: ", as Wings' do.
		return nil, nil, err.Error()
	}
	if claims.ServerUUID != s.uuid {
		return nil, nil, "jwt: server uuid mismatch"
	}
	s.mu.Lock()
	token := s.agentToken
	s.mu.Unlock()
	resigned, err = jwtx.Resign(raw, []byte(token))
	if err != nil {
		return nil, nil, "jwt: re-sign failed"
	}
	return claims, resigned, ""
}

// setState turns a power request into a spec change after checking the
// token's permissions the way Wings does.
func (s *session) setState(ctx context.Context, action string) {
	s.mu.Lock()
	claims := s.claims
	s.mu.Unlock()
	if claims == nil {
		_ = s.writeClient(Message{Event: "jwt error", Args: []string{"jwt: no jwt present"}})
		return
	}
	if claims.ExpirationTime != nil && time.Now().After(claims.ExpirationTime.Time) {
		_ = s.writeClient(Message{Event: "jwt error", Args: []string{"jwt: token expired"}})
		return
	}
	perm, ok := powerPermissions[action]
	if !ok {
		return
	}
	if !claims.HasPermission(perm) {
		return
	}
	if action == "start" || action == "restart" {
		if gs, err := s.p.Store.Get(ctx, s.uuid); err == nil {
			if st, err := settings.Parse(gs.Spec.Panel.Settings); err == nil && st.Suspended {
				_ = s.writeClient(Message{Event: "daemon error", Args: []string{"Cannot start or restart a server that is suspended."}})
				return
			}
		}
	}
	if err := s.p.Sync.Power(ctx, s.uuid, action); err != nil {
		s.p.Log.Warn("websocket power request failed", "uuid", s.uuid, "action", action, "error", err)
		_ = s.writeClient(Message{Event: "daemon error", Args: []string{"failed to record power action"}})
	}
}
