package sftprelay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/remote"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	srvUUID   = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	srvNS     = "pelican-servers"
	nodeToken = "node-token-secret"
)

func mustSigner(t *testing.T) ssh.Signer {
	t.Helper()
	pemBytes, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseHostKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHostKeyRoundTrip(t *testing.T) {
	pemBytes, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pemBytes), "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("not PEM: %q", pemBytes)
	}
	s, err := ParseHostKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %s", s.PublicKey().Type())
	}
	if _, err := ParseHostKey([]byte("garbage")); err == nil {
		t.Error("garbage must not parse")
	}
	other, _ := GenerateHostKey()
	if string(other) == string(pemBytes) {
		t.Error("host keys must be unique")
	}
}

func TestDescribe(t *testing.T) {
	gs := &v1alpha1.GameServer{}
	gs.Status.Agent.SftpHostKey = "  SHA256:abc \n"
	if got := Describe(gs); got != "SHA256:abc" {
		t.Errorf("Describe = %q", got)
	}
}

func TestWingsAlgorithms(t *testing.T) {
	c := wingsAlgorithms()
	if len(c.KeyExchanges) == 0 || len(c.Ciphers) == 0 || len(c.MACs) == 0 {
		t.Fatalf("empty algorithm lists: %+v", c)
	}
}

// fakeSftpAgent is an SSH server standing in for the agent's Wings SFTP
// server. It accepts the relay's session credential, answers "subsystem"
// requests and echoes the channel data.
type fakeSftpAgent struct {
	addr     string
	key      ssh.Signer
	sessions *Sessions

	mu     sync.Mutex
	users  []string
	passes []string
}

func newFakeSftpAgent(t *testing.T, sessions *Sessions) *fakeSftpAgent {
	t.Helper()
	a := &fakeSftpAgent{key: mustSigner(t), sessions: sessions}
	conf := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			a.mu.Lock()
			a.users = append(a.users, c.User())
			a.passes = append(a.passes, string(pw))
			a.mu.Unlock()
			if _, ok := sessions.Lookup(c.User(), string(pw)); !ok {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, nil
		},
	}
	conf.AddHostKey(a.key)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	a.addr = ln.Addr().String()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go a.serve(nc, conf)
		}
	}()
	return a
}

func (a *fakeSftpAgent) serve(nc net.Conn, conf *ssh.ServerConfig) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range creqs {
				if req.Type != "subsystem" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				go func() {
					_, _ = io.Copy(ch, ch)
					_, _ = ch.SendRequest("keepalive@test", false, nil) // agent-originated request
					ch.Close()
				}()
			}
		}()
	}
}

type relayEnv struct {
	t        *testing.T
	relay    *Relay
	addr     string
	agent    *fakeSftpAgent
	store    *store.Store
	panel    *fakepanel.Panel
	panelReq chan map[string]any
	cancel   context.CancelFunc
	done     chan error
}

type relayOpts struct {
	keyOnly    bool
	noGS       bool
	noPod      bool
	pinnedKey  string
	panelUUID  string
	rejectAuth bool
}

func newRelayEnv(t *testing.T, o relayOpts) *relayEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{})
	if !o.noGS {
		gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: names.ForUUID(srvUUID), Namespace: srvNS}}
		gs.Status.Agent.SftpHostKey = o.pinnedKey
		b = b.WithObjects(gs)
	}
	if !o.noPod {
		started := true
		b = b.WithObjects(
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: names.Pod(srvUUID), Namespace: srvNS},
				Status: corev1.PodStatus{
					PodIP:                 "127.0.0.1",
					InitContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}},
				},
			},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(srvUUID), Namespace: srvNS}, Data: map[string][]byte{"token_id": []byte("a"), "token": []byte("b")}},
		)
	}
	c := b.Build()
	st := store.New(c, srvNS, "default")

	e := &relayEnv{t: t, store: st, panelReq: make(chan map[string]any, 8)}
	e.panel = fakepanel.New("nodeid", nodeToken)
	if !o.rejectAuth {
		respUUID := srvUUID
		if o.panelUUID != "" {
			respUUID = o.panelUUID
		}
		e.panel.SftpAuth = func(req map[string]any) (map[string]any, bool) {
			e.panelReq <- req
			return map[string]any{"server": respUUID, "user": "user-1", "permissions": []string{"file.read"}}, true
		}
	}
	ps := httptest.NewServer(e.panel.Handler())
	t.Cleanup(ps.Close)

	sessions := NewSessions(nodeToken)
	e.agent = newFakeSftpAgent(t, sessions)

	// Reserve a free port for the relay to listen on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = l.Addr().String()
	l.Close()

	e.relay = &Relay{
		Listen:   e.addr,
		HostKey:  mustSigner(t),
		Panel:    panel.New(ps.URL, "nodeid", nodeToken, "Pelican Wings/v1.0.0 (id:nodeid)"),
		Store:    st,
		Agents:   agents.NewResolver(st, time.Second),
		Sessions: sessions,
		KeyOnly:  o.keyOnly,
		Log:      slog.Default(),
		dial: func(network, _ string) (net.Conn, error) {
			return net.DialTimeout(network, e.agent.addr, 5*time.Second)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.relay.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Error("relay did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", e.addr)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return e
}

func (e *relayEnv) connect(user string, auth ssh.AuthMethod) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(e.relay.HostKey.PublicKey()),
		Timeout:         5 * time.Second,
	})
}

// echoThroughRelay opens a session, starts the sftp subsystem and checks that
// bytes travel to the agent and back.
func echoThroughRelay(t *testing.T, c *ssh.Client) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	if _, err := in.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	_ = in.Close()
	// Closing our side reaches the agent, whose close comes back as EOF.
	if rest, err := io.ReadAll(out); err != nil || len(rest) != 0 {
		t.Errorf("expected clean EOF, got %q, %v", rest, err)
	}
}

func TestRelayPasswordLogin(t *testing.T) {
	e := newRelayEnv(t, relayOpts{})
	user := "alice." + srvUUID[:8]
	c, err := e.connect(user, ssh.Password("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	req := <-e.panelReq
	if req["type"] != "password" || req["username"] != user || req["password"] != "hunter2" {
		t.Errorf("panel request = %+v", req)
	}
	echoThroughRelay(t, c)

	// The agent saw the sealed session credential, not the user's password.
	e.agent.mu.Lock()
	defer e.agent.mu.Unlock()
	if len(e.agent.passes) != 1 || !strings.HasPrefix(e.agent.passes[0], sessionPrefix) || e.agent.users[0] != user {
		t.Errorf("agent saw users=%v passes=%v", e.agent.users, e.agent.passes)
	}
	resp, ok := e.agent.sessions.Lookup(user, e.agent.passes[0])
	if !ok || resp.Server != srvUUID || resp.User != "user-1" {
		t.Errorf("credential content: %+v %v", resp, ok)
	}
}

func TestRelayPinsAgentHostKey(t *testing.T) {
	e := newRelayEnv(t, relayOpts{})
	user := "alice." + srvUUID[:8]
	for i := 0; i < 2; i++ {
		c, err := e.connect(user, ssh.Password("pw"))
		if err != nil {
			t.Fatal(err)
		}
		echoThroughRelay(t, c)
		c.Close()
	}
	gs, err := e.store.Get(context.Background(), srvUUID)
	if err != nil {
		t.Fatal(err)
	}
	if want := ssh.FingerprintSHA256(e.agent.key.PublicKey()); gs.Status.Agent.SftpHostKey != want {
		t.Fatalf("pinned %q, want %q", gs.Status.Agent.SftpHostKey, want)
	}
}

func TestRelayRejectsChangedAgentHostKey(t *testing.T) {
	e := newRelayEnv(t, relayOpts{pinnedKey: "SHA256:somethingelse"})
	c, err := e.connect("alice."+srvUUID[:8], ssh.Password("pw"))
	if err != nil {
		t.Fatalf("client auth should succeed before the agent is dialed: %v", err)
	}
	defer c.Close()
	// The relay drops the connection because it cannot trust the agent.
	if sess, err := c.NewSession(); err == nil {
		sess.Close()
		t.Fatal("session opened against an agent with a different host key")
	}
	e.agent.mu.Lock()
	defer e.agent.mu.Unlock()
	if len(e.agent.passes) != 0 {
		t.Error("the credential must not be sent to an agent that failed host key verification")
	}
}

func TestRelayPublicKeyLogin(t *testing.T) {
	e := newRelayEnv(t, relayOpts{})
	priv := mustSigner(t)
	c, err := e.connect("alice."+srvUUID[:8], ssh.PublicKeys(priv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := <-e.panelReq
	if req["type"] != "public_key" || !strings.HasPrefix(req["password"].(string), "ssh-ed25519 ") {
		t.Errorf("panel request = %+v", req)
	}
	echoThroughRelay(t, c)
}

func TestRelayAuthFailures(t *testing.T) {
	good := "alice." + srvUUID[:8]
	for _, tc := range []struct {
		name string
		opts relayOpts
		user string
	}{
		{"password auth disabled", relayOpts{keyOnly: true}, good},
		{"invalid username format", relayOpts{}, "alice"},
		{"panel rejects", relayOpts{rejectAuth: true}, good},
		{"server not on this node", relayOpts{noGS: true}, good},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRelayEnv(t, tc.opts)
			c, err := e.connect(tc.user, ssh.Password("pw"))
			if err == nil {
				c.Close()
				t.Fatal("login must fail")
			}
			if !strings.Contains(err.Error(), "unable to authenticate") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRelayKeyOnlyStillAllowsKeys(t *testing.T) {
	e := newRelayEnv(t, relayOpts{keyOnly: true})
	c, err := e.connect("alice."+srvUUID[:8], ssh.PublicKeys(mustSigner(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echoThroughRelay(t, c)
}

func TestRelayAgentUnavailable(t *testing.T) {
	e := newRelayEnv(t, relayOpts{noPod: true})
	c, err := e.connect("alice."+srvUUID[:8], ssh.Password("pw"))
	if err != nil {
		t.Fatalf("client auth does not depend on the pod: %v", err)
	}
	defer c.Close()
	if sess, err := c.NewSession(); err == nil {
		sess.Close()
		t.Fatal("session opened without an agent")
	}
}

func TestRelayAgentDialFailure(t *testing.T) {
	e := newRelayEnv(t, relayOpts{})
	e.relay.dial = func(string, string) (net.Conn, error) { return nil, io.ErrClosedPipe }
	c, err := e.connect("alice."+srvUUID[:8], ssh.Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if sess, err := c.NewSession(); err == nil {
		sess.Close()
		t.Fatal("session opened without a reachable agent")
	}
}

func TestRelayChannelsAndRequests(t *testing.T) {
	e := newRelayEnv(t, relayOpts{})
	c, err := e.connect("alice."+srvUUID[:8], ssh.Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Only session channels are relayed.
	if ch, _, err := c.OpenChannel("direct-tcpip", nil); err == nil {
		ch.Close()
		t.Fatal("non-session channel accepted")
	} else if oce := new(*ssh.OpenChannelError); !errors.As(err, oce) {
		t.Fatalf("want OpenChannelError, got %T %v", err, err)
	}

	// A request the agent refuses is refused for the client too.
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Run("rm -rf /"); err == nil {
		t.Fatal("exec must be refused")
	}
	sess.Close()

	// Several sequential sessions work over one connection.
	echoThroughRelay(t, c)
	echoThroughRelay(t, c)
}

func TestRunListenError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r := &Relay{Listen: l.Addr().String(), HostKey: mustSigner(t), Log: slog.Default()}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("listening on a taken address must fail")
	}
}

func TestSessionsIssueRoundTripThroughRemote(t *testing.T) {
	// Credentials carry the Panel's answer verbatim.
	s := NewSessions(nodeToken)
	resp := &remote.SftpAuthResponse{Server: srvUUID, User: "u", Permissions: nil}
	got, ok := s.Lookup("x.12345678", s.Issue("x.12345678", resp))
	if !ok || got.Server != srvUUID || got.User != "u" {
		t.Fatalf("got %+v %v", got, ok)
	}
	// An empty server is never valid, whatever the MAC says.
	if _, ok := s.Lookup("x.12345678", s.Issue("x.12345678", &remote.SftpAuthResponse{})); ok {
		t.Fatal("credential without a server accepted")
	}
}
