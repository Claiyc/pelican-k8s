package wsproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"

	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
)

const (
	testUUID       = "11111111-2222-3333-4444-555555555555"
	testNodeToken  = "node-token"
	testAgentToken = "agent-token"
)

// wsPair returns both ends of a websocket connection: the end a server
// accepted and the end a client dialed.
func wsPair(t *testing.T) (accepted, dialed *websocket.Conn) {
	t.Helper()
	ch := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ch <- c
	}))
	t.Cleanup(srv.Close)
	dialed, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	accepted = <-ch
	t.Cleanup(func() { _ = accepted.Close(); _ = dialed.Close() })
	return accepted, dialed
}

// newTestSession wires a session to two in-memory websocket links and returns
// the browser's and the agent's end of them.
func newTestSession(t *testing.T) (s *session, browser, agent *websocket.Conn) {
	t.Helper()
	clientConn, browser := wsPair(t)
	agentConn, agent := wsPair(t)
	p := &Proxy{Cfg: &config.Config{NodeToken: testNodeToken}, grace: time.Millisecond, pollEvery: time.Millisecond}
	return &session{p: p, uuid: testUUID, agentToken: testAgentToken, client: clientConn, agent: agentConn}, browser, agent
}

type tokenClaims struct {
	jwt.Payload
	ServerUUID  string   `json:"server_uuid"`
	UserUUID    string   `json:"user_uuid"`
	Permissions []string `json:"permissions"`
}

func signToken(t *testing.T, key, uuid string, exp time.Time) string {
	t.Helper()
	tok, err := jwt.Sign(tokenClaims{
		Payload:     jwt.Payload{ExpirationTime: jwt.NumericDate(exp)},
		ServerUUID:  uuid,
		UserUUID:    "user",
		Permissions: []string{"websocket.connect", "control.start"},
	}, jwt.NewHS256([]byte(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(tok)
}

func readMessage(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var m Message
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func expectNothing(t *testing.T, c *websocket.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, data, err := c.ReadMessage(); err == nil {
		t.Fatalf("unexpected message %s", data)
	}
}

func TestHandleAuthRejectsWithJWTError(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	tests := []struct {
		name  string
		token string
		// want is the exact error text, or a prefix when prefixOnly is set.
		want       string
		prefixOnly bool
	}{
		{"bad signature", signToken(t, "other-key", testUUID, future), "jwt: ", true},
		{"garbage", "not-a-jwt", "jwt: ", true},
		{"expired", signToken(t, testNodeToken, testUUID, time.Now().Add(-time.Minute)), "jwt: token expired", false},
		{"uuid mismatch", signToken(t, testNodeToken, "99999999-2222-3333-4444-555555555555", future), "jwt: server uuid mismatch", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, browser, agent := newTestSession(t)
			s.handleAuth(Message{Event: "auth", Args: []string{tt.token}})
			m := readMessage(t, browser)
			if m.Event != "jwt error" || len(m.Args) != 1 {
				t.Fatalf("browser got %+v, want one jwt error", m)
			}
			if (tt.prefixOnly && !strings.HasPrefix(m.Args[0], tt.want)) || (!tt.prefixOnly && m.Args[0] != tt.want) {
				t.Fatalf("browser got %q, want %q", m.Args[0], tt.want)
			}
			// Wings sends its JWT errors as they are; the text must not carry the
			// "jwt: " prefix twice.
			if strings.Contains(m.Args[0], "jwt: jwt:") {
				t.Fatalf("browser got %q with a doubled prefix", m.Args[0])
			}
			expectNothing(t, agent)
			if s.claims != nil {
				t.Fatalf("claims stored for a rejected token: %+v", s.claims)
			}
		})
	}
}

func TestHandleAuthForwardsResignedToken(t *testing.T) {
	s, browser, agent := newTestSession(t)
	tok := signToken(t, testNodeToken, testUUID, time.Now().Add(10*time.Minute))

	s.handleAuth(Message{Event: "auth", Args: []string{tok}})

	m := readMessage(t, agent)
	if m.Event != "auth" || len(m.Args) != 1 {
		t.Fatalf("agent got %+v, want one auth arg", m)
	}
	if _, _, err := jwtx.Verify([]byte(m.Args[0]), []byte(testNodeToken)); err == nil {
		t.Fatal("forwarded token must not verify with the node token")
	}
	claims, _, err := jwtx.Verify([]byte(m.Args[0]), []byte(testAgentToken))
	if err != nil {
		t.Fatalf("forwarded token does not verify with the agent token: %v", err)
	}
	if claims.ServerUUID != testUUID || claims.UserUUID != "user" || !claims.HasPermission("control.start") {
		t.Fatalf("claims changed while re-signing: %+v", claims)
	}
	if s.claims == nil || s.claims.ServerUUID != testUUID || !s.claims.HasPermission("control.start") {
		t.Fatalf("session claims not stored: %+v", s.claims)
	}
	expectNothing(t, browser)
}

// While the agent is being replaced an auth frame is only verified: its claims
// are kept for power requests and nothing is forwarded.
func TestHandleAuthWithoutAgent(t *testing.T) {
	s, browser, agent := newTestSession(t)
	s.detach()
	expectClose(t, agent, websocket.CloseNormalClosure)
	tok := signToken(t, testNodeToken, testUUID, time.Now().Add(10*time.Minute))
	s.handleAuth(Message{Event: "auth", Args: []string{tok}})
	if s.claims == nil || s.claims.ServerUUID != testUUID {
		t.Fatalf("session claims not stored: %+v", s.claims)
	}
	expectNothing(t, browser)
}

// TestPumpClientToAgent covers the loop around handleAuth: auth is re-signed,
// other frames pass through unchanged, and the pump ends with the browser.
func TestPumpClientToAgent(t *testing.T) {
	s, browser, agent := newTestSession(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pumpClientToAgent(context.Background())
	}()

	tok := signToken(t, testNodeToken, testUUID, time.Now().Add(10*time.Minute))
	authFrame, _ := json.Marshal(Message{Event: "auth", Args: []string{tok}})
	if err := browser.WriteMessage(websocket.TextMessage, authFrame); err != nil {
		t.Fatal(err)
	}
	if m := readMessage(t, agent); m.Event != "auth" || m.Args[0] == tok {
		t.Fatalf("agent got %+v, want a re-signed auth", m)
	}

	const cmd = `{"event":"send command","args":["say hi"]}`
	if err := browser.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		t.Fatal(err)
	}
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := agent.ReadMessage(); err != nil || string(data) != cmd {
		t.Fatalf("agent got %q, %v; want the frame unchanged", data, err)
	}

	_ = browser.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop when the browser closed")
	}
}
