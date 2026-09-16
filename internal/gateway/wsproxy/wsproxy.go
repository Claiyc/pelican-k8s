// Package wsproxy relays console websockets between browsers and agents,
// re-signing auth tokens and turning power intents into spec changes
// (ARCHITECTURE.md 5.5, 8.3).
package wsproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
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

	t, err := p.Agents.Resolve(r.Context(), uuid)
	if err != nil {
		_ = client.WriteJSON(Message{Event: "daemon error", Args: []string{"server pod unavailable"}})
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "server pod unavailable"), time.Now().Add(time.Second))
		return
	}
	header := http.Header{}
	if p.OriginForAgent != "" {
		header.Set("Origin", p.OriginForAgent)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: true}
	agent, _, err := dialer.Dial("ws://"+t.PodIP+":8080/api/servers/"+uuid+"/ws", header)
	if err != nil {
		p.Log.Warn("agent websocket dial failed", "uuid", uuid, "error", err)
		_ = client.WriteJSON(Message{Event: "daemon error", Args: []string{"could not reach the server agent"}})
		return
	}
	defer agent.Close()

	session := &session{p: p, uuid: uuid, agentToken: t.Token, client: client, agent: agent}
	session.run(r.Context())
}

type session struct {
	p          *Proxy
	uuid       string
	agentToken string
	client     *websocket.Conn
	agent      *websocket.Conn

	mu     sync.Mutex
	claims *jwtx.Claims

	clientWriteMu sync.Mutex
	agentWriteMu  sync.Mutex
}

func (s *session) writeClient(v any) error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	return s.client.WriteJSON(v)
}

func (s *session) writeAgentRaw(mt int, data []byte) error {
	s.agentWriteMu.Lock()
	defer s.agentWriteMu.Unlock()
	return s.agent.WriteMessage(mt, data)
}

func (s *session) run(ctx context.Context) {
	done := make(chan struct{}, 2)
	// agent -> client: pass through.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := s.agent.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					s.clientWriteMu.Lock()
					_ = s.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.Text), time.Now().Add(time.Second))
					s.clientWriteMu.Unlock()
				}
				return
			}
			s.clientWriteMu.Lock()
			err = s.client.WriteMessage(mt, data)
			s.clientWriteMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	// client -> agent: inspect auth and set state.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := s.client.ReadMessage()
			if err != nil {
				s.agentWriteMu.Lock()
				_ = s.agent.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				s.agentWriteMu.Unlock()
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var m Message
			if err := json.Unmarshal(data, &m); err != nil {
				_ = s.writeAgentRaw(mt, data)
				continue
			}
			switch m.Event {
			case "auth":
				claims, raw, err := jwtx.Verify([]byte(strings.Join(m.Args, "")), []byte(s.p.Cfg.NodeToken))
				if err != nil {
					_ = s.writeClient(Message{Event: "jwt error", Args: []string{"jwt: " + err.Error()}})
					continue
				}
				if claims.ServerUUID != s.uuid {
					_ = s.writeClient(Message{Event: "jwt error", Args: []string{"jwt: server uuid mismatch"}})
					continue
				}
				resigned, err := jwtx.Resign(raw, []byte(s.agentToken))
				if err != nil {
					_ = s.writeClient(Message{Event: "jwt error", Args: []string{"jwt: re-sign failed"}})
					continue
				}
				s.mu.Lock()
				s.claims = claims
				s.mu.Unlock()
				out, _ := json.Marshal(Message{Event: "auth", Args: []string{string(resigned)}})
				if err := s.writeAgentRaw(websocket.TextMessage, out); err != nil {
					return
				}
			case "set state":
				s.setState(ctx, strings.Join(m.Args, ""))
			default:
				if err := s.writeAgentRaw(mt, data); err != nil {
					return
				}
			}
		}
	}()
	<-done
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
