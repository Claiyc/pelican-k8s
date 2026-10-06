// Package sftprelay terminates user SSH sessions, authenticates them against
// the Panel and relays the SFTP subsystem to the agent's stock Wings SFTP
// server (ARCHITECTURE.md 5.6).
package sftprelay

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pelican/wings/remote"
	"golang.org/x/crypto/ssh"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
)

var validUsername = regexp.MustCompile(`^(?i)(.+)\.([a-z0-9]{8})$`)

// Sessions issues and verifies the credential the relay presents to the
// agent's Wings SFTP server as the SSH password. The credential carries the
// Panel's answer for the login (server, user, permissions), the SSH username
// and an expiry, sealed with an HMAC key derived from the node token. Every
// gateway replica derives the same key, so whichever replica receives the
// agent's /sftp/auth call can verify the credential without shared state.
type Sessions struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// sessionPrefix versions the credential format and the key derivation.
const sessionPrefix = "pks1."

// sessionTTL bounds how long a credential is accepted. The relay dials the
// agent right after the client authenticated, so this only has to cover the
// agent's SSH handshake.
const sessionTTL = 2 * time.Minute

type sessionClaims struct {
	Username    string   `json:"n"`
	Server      string   `json:"s"`
	User        string   `json:"u"`
	Permissions []string `json:"p"`
	Expires     int64    `json:"e"`
	Nonce       string   `json:"r"`
}

// NewSessions derives the credential key from the node token.
func NewSessions(nodeToken string) *Sessions {
	m := hmac.New(sha256.New, []byte(nodeToken))
	m.Write([]byte("pelican-k8s sftp session credential v1"))
	return &Sessions{key: m.Sum(nil), ttl: sessionTTL, now: time.Now}
}

func (s *Sessions) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(sessionPrefix))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Issue creates the credential for a login the Panel accepted.
func (s *Sessions) Issue(username string, resp *remote.SftpAuthResponse) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	b, _ := json.Marshal(sessionClaims{
		Username:    username,
		Server:      resp.Server,
		User:        resp.User,
		Permissions: resp.Permissions,
		Expires:     s.now().Add(s.ttl).Unix(),
		Nonce:       base64.RawURLEncoding.EncodeToString(nonce),
	})
	payload := base64.RawURLEncoding.EncodeToString(b)
	return sessionPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

// Lookup implements remoteapi.SftpSessions: it returns the Panel's answer
// sealed in the credential when the MAC verifies, the credential has not
// expired and it was issued for this SSH username.
func (s *Sessions) Lookup(username, password string) (*remote.SftpAuthResponse, bool) {
	rest, ok := strings.CutPrefix(password, sessionPrefix)
	if !ok {
		return nil, false
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, false
	}
	var c sessionClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, false
	}
	if !s.now().Before(time.Unix(c.Expires, 0)) || c.Username != username || c.Server == "" {
		return nil, false
	}
	return &remote.SftpAuthResponse{Server: c.Server, User: c.User, Permissions: c.Permissions}, true
}

// Relay is the SFTP relay server.
type Relay struct {
	Listen   string
	HostKey  ssh.Signer
	Panel    *panel.Client
	Store    *store.Store
	Agents   *agents.Resolver
	Sessions *Sessions
	KeyOnly  bool
	Log      *slog.Logger
	// TrustedProxyIPs, when the listener sits behind a PROXY-protocol-less LB, is unused; client IPs are the TCP peer.
}

// wingsAlgorithms mirrors Wings' pinned algorithms.
func wingsAlgorithms() ssh.Config {
	return ssh.Config{
		KeyExchanges: []string{"curve25519-sha256", "curve25519-sha256@libssh.org", "ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521", "diffie-hellman-group14-sha256"},
		Ciphers:      []string{"aes128-gcm@openssh.com", "chacha20-poly1305@openssh.com", "aes128-ctr", "aes192-ctr", "aes256-ctr"},
		MACs:         []string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256"},
	}
}

// Run serves until ctx ends.
func (r *Relay) Run(ctx context.Context) error {
	conf := &ssh.ServerConfig{
		Config:       wingsAlgorithms(),
		MaxAuthTries: 6,
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if r.KeyOnly {
				return nil, errors.New("password authentication is disabled")
			}
			return r.authenticate(ctx, conn, remote.SftpAuthPassword, string(password))
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return r.authenticate(ctx, conn, remote.SftpAuthPublicKey, string(ssh.MarshalAuthorizedKey(key)))
		},
	}
	conf.AddHostKey(r.HostKey)
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	r.Log.Info("sftp relay listening", "addr", r.Listen, "hostkey", ssh.FingerprintSHA256(r.HostKey.PublicKey()))
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go r.handle(ctx, conn, conf)
	}
}

func (r *Relay) authenticate(ctx context.Context, conn ssh.ConnMetadata, typ remote.SftpAuthRequestType, secret string) (*ssh.Permissions, error) {
	user := conn.User()
	if !validUsername.MatchString(user) {
		return nil, errors.New("invalid username format")
	}
	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	req := remote.SftpAuthRequest{Type: typ, User: user, Pass: secret, IP: conn.RemoteAddr().String(), SessionID: conn.SessionID(), ClientVersion: conn.ClientVersion()}
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := r.Panel.ValidateSftpCredentials(actx, req)
	if err != nil {
		r.Log.Warn("sftp auth rejected", "user", user, "ip", ip, "error", err)
		return nil, errors.New("invalid credentials")
	}
	if !r.Store.Exists(ctx, resp.Server) {
		return nil, errors.New("server not on this node")
	}
	cred := r.Sessions.Issue(user, resp)
	return &ssh.Permissions{Extensions: map[string]string{"uuid": resp.Server, "user": resp.User, "session": cred}}, nil
}

func (r *Relay) handle(ctx context.Context, nconn net.Conn, conf *ssh.ServerConfig) {
	defer nconn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(nconn, conf)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)
	uuid := sconn.Permissions.Extensions["uuid"]
	cred := sconn.Permissions.Extensions["session"]

	target, err := r.Agents.Resolve(ctx, uuid)
	if err != nil {
		r.Log.Warn("sftp: agent unavailable", "uuid", uuid, "error", err)
		return
	}
	agentConn, err := r.dialAgent(ctx, target, sconn.User(), cred)
	if err != nil {
		r.Log.Warn("sftp: agent dial failed", "uuid", uuid, "error", err)
		return
	}
	defer func() { _ = agentConn.Close() }()

	var wg sync.WaitGroup
	defer wg.Wait()
	for ch := range chans {
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		clientCh, clientReqs, err := ch.Accept()
		if err != nil {
			continue
		}
		agentCh, agentReqs, err := agentConn.OpenChannel("session", nil)
		if err != nil {
			clientCh.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			relayChannel(clientCh, clientReqs, agentCh, agentReqs)
		}()
	}
}

// dialAgent opens the SSH connection to the agent's Wings SFTP server using
// the session credential, pinning the agent host key on first use.
func (r *Relay) dialAgent(ctx context.Context, t *agents.Target, user, cred string) (*ssh.Client, error) {
	gs, err := r.Store.Get(ctx, t.UUID)
	if err != nil {
		return nil, err
	}
	pinned := gs.Status.Agent.SftpHostKey
	cfg := &ssh.ClientConfig{
		Config:  wingsAlgorithms(),
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.Password(cred)},
		Timeout: 10 * time.Second,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if pinned == "" {
				_ = r.Store.PatchStatus(ctx, t.UUID, map[string]any{"agent": map[string]any{"sftpHostKey": fp}})
				return nil
			}
			if fp != pinned {
				return fmt.Errorf("agent host key mismatch: %s != %s", fp, pinned)
			}
			return nil
		},
	}
	return ssh.Dial("tcp", t.SFTPAddr(), cfg)
}

// relayChannel forwards requests and bytes between a client session channel
// and the agent session channel.
func relayChannel(client ssh.Channel, clientReqs <-chan *ssh.Request, agent ssh.Channel, agentReqs <-chan *ssh.Request) {
	defer client.Close()
	defer agent.Close()
	go func() {
		for req := range clientReqs {
			ok, err := agent.SendRequest(req.Type, req.WantReply, req.Payload)
			if err != nil {
				ok = false
			}
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
		}
	}()
	go func() {
		for req := range agentReqs {
			ok, _ := client.SendRequest(req.Type, req.WantReply, req.Payload)
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(agent, client)
		_ = agent.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, agent)
		_ = client.CloseWrite()
	}()
	wg.Wait()
}

// GenerateHostKey creates a PEM-encoded ED25519 private key.
func GenerateHostKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), nil
}

// ParseHostKey parses a PEM private key.
func ParseHostKey(pemBytes []byte) (ssh.Signer, error) {
	return ssh.ParsePrivateKey(pemBytes)
}

// Describe is used by diagnostics.
func Describe(gs *v1alpha1.GameServer) string {
	return strings.TrimSpace(gs.Status.Agent.SftpHostKey)
}
