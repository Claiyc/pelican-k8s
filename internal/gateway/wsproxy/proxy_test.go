package wsproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	uuid      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	ns        = "pelican-servers"
	nodeID    = "nodeid"
	nodeToken = "node-token-secret"
	agentTok  = "agent-token-secret"
)

type env struct {
	t        *testing.T
	c        client.Client
	st       *store.Store
	proxy    *Proxy
	srv      *httptest.Server // serves the proxy
	agentSrv *httptest.Server

	agentOrigin chan string
	agentConn   chan *websocket.Conn
	agentRecv   chan string
}

type opts struct {
	noServer  bool
	noPod     bool
	suspended bool
	agentDown bool
	panelDown bool
	origins   []string
	agentOrig string
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(cls).Build()

	fp := fakepanel.New(nodeID, nodeToken)
	fp.Add(&fakepanel.Server{UUID: uuid, Settings: fakepanel.PaperSettings(uuid, 30565, 0), ProcessConfiguration: fakepanel.PaperProcessConfiguration()})
	ps := httptest.NewServer(fp.Handler())
	t.Cleanup(ps.Close)
	panelURL := ps.URL
	if o.panelDown {
		panelURL = "http://127.0.0.1:1"
	}
	cfg := &config.Config{PanelURL: panelURL, NodeTokenID: nodeID, NodeToken: nodeToken, ServersNamespace: ns, DefaultClass: "default", AdvertisedVersion: "1.0.0", AllowedOrigins: o.origins, Timezone: "UTC", StateCacheTTL: time.Second}
	st := store.New(c, ns, "default")
	pc := panel.New(ps.URL, nodeID, nodeToken, cfg.UserAgent())
	sy := &serversync.Syncer{Store: st, Panel: pc, Timezone: "UTC", Log: slog.Default()}
	if !o.noServer {
		if err := sy.Create(context.Background(), uuid, false); err != nil {
			t.Fatal(err)
		}
	}
	if o.panelDown {
		sy.Panel = panel.New(panelURL, nodeID, nodeToken, cfg.UserAgent())
	}
	if o.suspended {
		gs, err := st.Get(context.Background(), uuid)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(gs.Spec.Panel.Settings.Raw, &m)
		m["suspended"] = true
		gs.Spec.Panel.Settings.Raw, _ = json.Marshal(m)
		if err := c.Update(context.Background(), gs); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, c: c, st: st, agentOrigin: make(chan string, 4), agentConn: make(chan *websocket.Conn, 4), agentRecv: make(chan string, 64)}
	if !o.noPod && !o.noServer {
		started := true
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns}}
		if err := c.Create(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		pod.Status = corev1.PodStatus{PodIP: "127.0.0.1", InitContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}}}
		if err := c.Status().Update(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns}, Data: map[string][]byte{"token_id": []byte("agentid"), "token": []byte(agentTok)}}
		if err := c.Create(context.Background(), sec); err != nil {
			t.Fatal(err)
		}
	}

	// Fake agent websocket endpoint.
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	e.agentSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		e.agentOrigin <- r.Header.Get("Origin")
		e.agentConn <- conn
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				e.agentRecv <- "closed: " + err.Error()
				return
			}
			e.agentRecv <- string(data)
		}
	}))
	t.Cleanup(e.agentSrv.Close)
	agentAddr := e.agentSrv.Listener.Addr().String()

	res := agents.NewResolver(st, time.Second)
	e.proxy = &Proxy{Cfg: cfg, Store: st, Agents: res, Sync: sy, Log: slog.Default(), OriginForAgent: o.agentOrig,
		netDial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if o.agentDown {
				return nil, errors.New("agent down")
			}
			return (&net.Dialer{}).DialContext(ctx, network, agentAddr)
		}}
	mux := http.NewServeMux()
	mux.Handle("GET /api/servers/{server}/ws", e.proxy)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) dial(uuid string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/servers/" + uuid + "/ws"
	return websocket.DefaultDialer.Dial(url, hdr)
}

func (e *env) connect() *websocket.Conn {
	e.t.Helper()
	conn, res, err := e.dial(uuid, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	e.t.Cleanup(func() { conn.Close() })
	return conn
}

func (e *env) waitAgent() *websocket.Conn {
	e.t.Helper()
	select {
	case c := <-e.agentConn:
		e.t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(5 * time.Second):
		e.t.Fatal("agent never connected")
		return nil
	}
}

func (e *env) nextAgent() string {
	e.t.Helper()
	select {
	case m := <-e.agentRecv:
		return m
	case <-time.After(5 * time.Second):
		e.t.Fatal("timeout waiting for a frame at the agent")
		return ""
	}
}

func readMsg(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m Message
	if err := c.ReadJSON(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func send(t *testing.T, c *websocket.Conn, event string, args ...string) {
	t.Helper()
	if err := c.WriteJSON(Message{Event: event, Args: args}); err != nil {
		t.Fatal(err)
	}
}

func token(t *testing.T, key string, claims map[string]any) string {
	t.Helper()
	b, err := jwt.Sign(claims, jwt.NewHS256([]byte(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func validClaims(perms ...string) map[string]any {
	return map[string]any{
		"server_uuid": uuid,
		"user_uuid":   "user-1",
		"permissions": perms,
		"scope":       "websocket",
		"exp":         time.Now().Add(time.Hour).Unix(),
	}
}

func (e *env) gsPower() v1alpha1.PowerSpec {
	e.t.Helper()
	gs, err := e.st.Get(context.Background(), uuid)
	if err != nil {
		e.t.Fatal(err)
	}
	return gs.Spec.Power
}

func TestCheckOrigin(t *testing.T) {
	p := &Proxy{Cfg: &config.Config{PanelURL: "https://panel.example.com", AllowedOrigins: []string{"https://extra.example"}}}
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"https://panel.example.com", true},
		{"https://extra.example", true},
		{"https://evil.example", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := p.checkOrigin(r); got != tc.want {
			t.Errorf("origin %q: got %v, want %v", tc.origin, got, tc.want)
		}
	}
	p.Cfg.AllowedOrigins = []string{"*"}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "https://anything.example")
	if !p.checkOrigin(r) {
		t.Error("wildcard origin must allow everything")
	}
}

func TestUnknownServer(t *testing.T) {
	e := newEnv(t, opts{noServer: true})
	_, res, err := e.dial(uuid, nil)
	if err == nil || res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 handshake failure, got %v %v", res, err)
	}
	res.Body.Close()
}

func TestOriginRejected(t *testing.T) {
	e := newEnv(t, opts{})
	_, res, err := e.dial(uuid, http.Header{"Origin": {"https://evil.example"}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 handshake failure, got %v %v", res, err)
	}
	res.Body.Close()
	e = newEnv(t, opts{origins: []string{"https://ok.example"}})
	conn, res, err := e.dial(uuid, http.Header{"Origin": {"https://ok.example"}})
	if err != nil {
		t.Fatalf("allowed origin rejected: %v", err)
	}
	res.Body.Close()
	conn.Close()
}

func TestSuspendedServerIsClosed(t *testing.T) {
	e := newEnv(t, opts{suspended: true})
	conn := e.connect()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4409 {
		t.Fatalf("want close 4409, got %v", err)
	}
}

func TestPodUnavailable(t *testing.T) {
	e := newEnv(t, opts{noPod: true})
	conn := e.connect()
	if m := readMsg(t, conn); m.Event != "daemon error" || m.Args[0] != "server pod unavailable" {
		t.Fatalf("got %+v", m)
	}
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseTryAgainLater {
		t.Fatalf("want close 1013, got %v", err)
	}
}

func TestAgentDialFailure(t *testing.T) {
	e := newEnv(t, opts{agentDown: true})
	conn := e.connect()
	if m := readMsg(t, conn); m.Event != "daemon error" || m.Args[0] != "could not reach the server agent" {
		t.Fatalf("got %+v", m)
	}
}

func TestAuthResignsForAgent(t *testing.T) {
	e := newEnv(t, opts{agentOrig: "http://gateway.test:8081"})
	conn := e.connect()
	agent := e.waitAgent()
	if got := <-e.agentOrigin; got != "http://gateway.test:8081" {
		t.Errorf("agent Origin = %q", got)
	}

	panelTok := token(t, nodeToken, validClaims("control.console"))
	send(t, conn, "auth", panelTok)
	var m Message
	if err := json.Unmarshal([]byte(e.nextAgent()), &m); err != nil {
		t.Fatal(err)
	}
	if m.Event != "auth" || len(m.Args) != 1 {
		t.Fatalf("agent got %+v", m)
	}
	claims, _, err := jwtx.Verify([]byte(m.Args[0]), []byte(agentTok))
	if err != nil {
		t.Fatalf("re-signed token does not verify with the agent token: %v", err)
	}
	if claims.ServerUUID != uuid || claims.UserUUID != "user-1" || !claims.HasPermission("control.console") {
		t.Errorf("claims not preserved: %+v", claims)
	}
	if _, _, err := jwtx.Verify([]byte(m.Args[0]), []byte(nodeToken)); err == nil {
		t.Error("re-signed token must not verify with the node token")
	}

	// Agent to client frames are passed through untouched, text and binary.
	if err := agent.WriteJSON(Message{Event: "console output", Args: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, conn); m.Event != "console output" || m.Args[0] != "hello" {
		t.Fatalf("got %+v", m)
	}
	if err := agent.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || string(data) != "\x01\x02\x03" {
		t.Fatalf("binary frame: %d %v %v", mt, data, err)
	}
}

func TestAuthRejections(t *testing.T) {
	e := newEnv(t, opts{})
	conn := e.connect()
	e.waitAgent()

	otherUUID := validClaims("*")
	otherUUID["server_uuid"] = "00000000-0000-4000-8000-000000000000"
	expired := validClaims("*")
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	for _, tc := range []struct{ name, tok, want string }{
		{"wrong key", token(t, "other-key", validClaims("*")), "jwt: "},
		{"garbage", "not-a-jwt", "jwt: "},
		{"expired", token(t, nodeToken, expired), "expired"},
		{"other server", token(t, nodeToken, otherUUID), "server uuid mismatch"},
	} {
		send(t, conn, "auth", tc.tok)
		m := readMsg(t, conn)
		if m.Event != "jwt error" || !strings.Contains(m.Args[0], tc.want) {
			t.Errorf("%s: got %+v", tc.name, m)
		}
	}
	// Nothing reached the agent; a following frame is the first it sees.
	send(t, conn, "send logs")
	if got := e.nextAgent(); !strings.Contains(got, "send logs") {
		t.Fatalf("agent saw %q first", got)
	}
}

func TestClientFramesPassThrough(t *testing.T) {
	e := newEnv(t, opts{})
	conn := e.connect()
	e.waitAgent()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json at all")); err != nil {
		t.Fatal(err)
	}
	if got := e.nextAgent(); got != "not json at all" {
		t.Fatalf("raw frame: %q", got)
	}
	send(t, conn, "send command", "say hi")
	var m Message
	if err := json.Unmarshal([]byte(e.nextAgent()), &m); err != nil || m.Event != "send command" || m.Args[0] != "say hi" {
		t.Fatalf("command frame: %+v %v", m, err)
	}
	// Binary frames from browsers are dropped.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("bin")); err != nil {
		t.Fatal(err)
	}
	send(t, conn, "send stats")
	if got := e.nextAgent(); !strings.Contains(got, "send stats") {
		t.Fatalf("binary frame was forwarded, agent saw %q", got)
	}
}

func TestCloseIsPropagated(t *testing.T) {
	t.Run("agent closes with a code", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		agent := e.waitAgent()
		_ = agent.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, "bye"), time.Now().Add(time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != 4001 || ce.Text != "bye" {
			t.Fatalf("want close 4001 bye, got %v", err)
		}
	})
	t.Run("client leaves", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		conn.Close()
		if got := e.nextAgent(); !strings.HasPrefix(got, "closed:") {
			t.Fatalf("agent connection should end, saw %q", got)
		}
	})
	t.Run("oversized client frame", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 8192)))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
		if got := e.nextAgent(); !strings.HasPrefix(got, "closed:") {
			t.Fatalf("agent connection should end, saw %q", got)
		}
	})
}

// waitPower polls the GameServer until the power spec satisfies ok.
func (e *env) waitPower(ok func(v1alpha1.PowerSpec) bool) v1alpha1.PowerSpec {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := e.gsPower()
		if ok(p) || time.Now().After(deadline) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSetState(t *testing.T) {
	t.Run("stop and kill become spec changes", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "auth", token(t, nodeToken, validClaims("control.start", "control.stop")))
		_ = e.nextAgent()

		send(t, conn, "set state", "start")
		p := e.waitPower(func(p v1alpha1.PowerSpec) bool { return p.Desired == v1alpha1.PowerRunning })
		if p.Desired != v1alpha1.PowerRunning || p.Kill {
			t.Fatalf("start -> %+v", p)
		}
		send(t, conn, "set state", "kill")
		p = e.waitPower(func(p v1alpha1.PowerSpec) bool { return p.Kill })
		if p.Desired != v1alpha1.PowerStopped || !p.Kill {
			t.Fatalf("kill -> %+v", p)
		}
		send(t, conn, "set state", "stop")
		p = e.waitPower(func(p v1alpha1.PowerSpec) bool { return !p.Kill })
		if p.Desired != v1alpha1.PowerStopped || p.Kill {
			t.Fatalf("stop -> %+v", p)
		}
	})

	t.Run("without permission or unknown action nothing changes", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "auth", token(t, nodeToken, validClaims("control.console")))
		_ = e.nextAgent()
		before := e.gsPower()
		send(t, conn, "set state", "start")
		send(t, conn, "set state", "explode")
		// Ordering: once the agent sees this frame both set-state frames were handled.
		send(t, conn, "send stats")
		_ = e.nextAgent()
		if after := e.gsPower(); after != before {
			t.Fatalf("power changed: %+v -> %+v", before, after)
		}
	})

	t.Run("wildcard grants control", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "auth", token(t, nodeToken, validClaims("*")))
		_ = e.nextAgent()
		send(t, conn, "set state", "restart")
		if p := e.waitPower(func(p v1alpha1.PowerSpec) bool { return p.Desired == v1alpha1.PowerRunning }); p.Desired != v1alpha1.PowerRunning {
			t.Fatalf("restart -> %+v", p)
		}
	})

	t.Run("no jwt", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "set state", "stop")
		if m := readMsg(t, conn); m.Event != "jwt error" || !strings.Contains(m.Args[0], "no jwt") {
			t.Fatalf("got %+v", m)
		}
	})

	t.Run("start of a suspended server is refused", func(t *testing.T) {
		e := newEnv(t, opts{})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "auth", token(t, nodeToken, validClaims("*")))
		_ = e.nextAgent()
		// Suspend after the session opened.
		gs, _ := e.st.Get(context.Background(), uuid)
		var m map[string]any
		_ = json.Unmarshal(gs.Spec.Panel.Settings.Raw, &m)
		m["suspended"] = true
		gs.Spec.Panel.Settings.Raw, _ = json.Marshal(m)
		if err := e.c.Update(context.Background(), gs); err != nil {
			t.Fatal(err)
		}
		before := e.gsPower()
		send(t, conn, "set state", "start")
		if msg := readMsg(t, conn); msg.Event != "daemon error" || !strings.Contains(msg.Args[0], "suspended") {
			t.Fatalf("got %+v", msg)
		}
		if e.gsPower() != before {
			t.Fatal("power must not change for a suspended server")
		}
	})

	t.Run("power failure is reported", func(t *testing.T) {
		e := newEnv(t, opts{panelDown: true})
		conn := e.connect()
		e.waitAgent()
		send(t, conn, "auth", token(t, nodeToken, validClaims("*")))
		_ = e.nextAgent()
		send(t, conn, "set state", "start") // start syncs with the unreachable Panel first
		if msg := readMsg(t, conn); msg.Event != "daemon error" || !strings.Contains(msg.Args[0], "failed to record power action") {
			t.Fatalf("got %+v", msg)
		}
	})
}

// A token that expires after authentication stops working for power actions.
func TestSetStateExpiredToken(t *testing.T) {
	e := newEnv(t, opts{})
	conn := e.connect()
	agent := e.waitAgent()
	_ = agent

	// Drive a session directly so the claims can be aged without sleeping.
	up := websocket.Upgrader{}
	var mu sync.Mutex
	var serverSide *websocket.Conn
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		serverSide = c
		mu.Unlock()
		close(ready)
	}))
	defer srv.Close()
	peer, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	defer peer.Close()
	<-ready
	mu.Lock()
	defer mu.Unlock()
	past := jwt.NumericDate(time.Now().Add(-time.Minute))
	s := &session{p: e.proxy, uuid: uuid, client: serverSide, claims: &jwtx.Claims{Payload: jwt.Payload{ExpirationTime: past}, Permissions: []string{"*"}}}
	before := e.gsPower()
	s.setState(context.Background(), "stop")
	if m := readMsg(t, peer); m.Event != "jwt error" || !strings.Contains(m.Args[0], "expired") {
		t.Fatalf("got %+v", m)
	}
	if e.gsPower() != before {
		t.Fatal("power changed with an expired token")
	}
	_ = conn
}
