// Package sftprelay terminates user SSH sessions, authenticates them against
// the Panel and relays the SFTP subsystem to the agent's stock Wings SFTP
// server (ARCHITECTURE.md 5.6).
package sftprelay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
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

// Sessions records relayed logins so the remote API can answer the agent's
// own /sftp/auth call with the Panel's response.
type Sessions struct {
	mu   sync.Mutex
	byID map[string]sessionRecord
}

type sessionRecord struct {
	username string
	resp     *remote.SftpAuthResponse
	expires  time.Time
}

// NewSessions returns an empty session store.
func NewSessions() *Sessions { return &Sessions{byID: map[string]sessionRecord{}} }

// Issue creates a one-time credential for the login.
func (s *Sessions) Issue(username string, resp *remote.SftpAuthResponse) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	cred := hex.EncodeToString(b)
	s.mu.Lock()
	s.byID[cred] = sessionRecord{username: username, resp: resp, expires: time.Now().Add(2 * time.Minute)}
	for k, v := range s.byID {
		if time.Now().After(v.expires) {
			delete(s.byID, k)
		}
	}
	s.mu.Unlock()
	return cred
}

// Lookup implements remoteapi.SftpSessions.
func (s *Sessions) Lookup(username, password string) (*remote.SftpAuthResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[password]
	if !ok || time.Now().After(rec.expires) || subtle.ConstantTimeCompare([]byte(rec.username), []byte(username)) != 1 {
		return nil, false
	}
	return rec.resp, true
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
// the one-time session credential, pinning the agent host key on first use.
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
