package protocol

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// TokenEnv names the environment variable that carries the shared shim token
// into the agent and the shim. The shim removes it from the environment of the
// game process and makes itself non-dumpable, so the game cannot read it.
const TokenEnv = "PELICAN_SHIM_TOKEN"

// Handshake message types. Every connection starts with a mutual challenge:
// the agent sends a nonce, the shim answers it and sends a nonce of its own
// with its pod UID, and the agent answers that.
const (
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
)

// HandshakeTimeout bounds the authentication of a new connection.
const HandshakeTimeout = 5 * time.Second

const challengeSize = 32

// Roles bind a proof to the side that computed it, so one side's answer can
// never be replayed as the other's.
const (
	roleShim  = "shim"
	roleAgent = "agent"
)

// Proof is HMAC-SHA256 keyed with the shared token over the role of the
// prover, the peer's challenge and the bound data (the shim's pod UID, or the
// agent's own challenge).
func Proof(token []byte, role string, challenge []byte, bound string) []byte {
	m := hmac.New(sha256.New, token)
	m.Write([]byte("pelican-k8s shim auth v2\x00"))
	m.Write([]byte(role))
	m.Write([]byte{0})
	m.Write(challenge)
	m.Write([]byte{0})
	m.Write([]byte(bound))
	return m.Sum(nil)
}

// ErrUnauthenticated is returned when a peer fails the handshake.
var ErrUnauthenticated = errors.New("protocol: peer failed authentication")

func nonce() ([]byte, error) {
	b := make([]byte, challengeSize)
	_, err := rand.Read(b)
	return b, err
}

// Answer runs the shim side of the handshake on a connection the shim dialed:
// it answers the agent's challenge, sends its pod UID and a challenge of its
// own, and checks the agent's answer. The returned decoder must be used for
// the rest of the connection, since it may already hold buffered bytes.
func Answer(conn net.Conn, enc *Encoder, token []byte, podUID string, timeout time.Duration) (*Decoder, error) {
	if len(token) == 0 {
		return nil, errors.New("protocol: empty shim token")
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	dec := NewDecoder(conn)
	var m Message
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("protocol: read challenge: %w", err)
	}
	if m.Type != TypeChallenge || len(m.Data) != challengeSize {
		return nil, fmt.Errorf("protocol: expected a challenge, got %q", m.Type)
	}
	agentChallenge := m.Data
	mine, err := nonce()
	if err != nil {
		return nil, err
	}
	if err := enc.Encode(&Message{Type: TypeAuth, Data: Proof(token, roleShim, agentChallenge, podUID), Challenge: mine, PodUID: podUID}); err != nil {
		return nil, err
	}
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("protocol: read the agent's proof: %w", err)
	}
	if m.Type != TypeAuth || !hmac.Equal(m.Data, Proof(token, roleAgent, mine, string(agentChallenge))) {
		return nil, ErrUnauthenticated
	}
	return dec, nil
}

// Challenge runs the agent side of the handshake on an accepted connection and
// returns a Client once the peer has proven knowledge of the token, after
// proving it in turn.
func Challenge(conn net.Conn, token []byte, timeout time.Duration) (*Client, error) {
	if len(token) == 0 {
		return nil, errors.New("protocol: empty shim token")
	}
	challenge, err := nonce()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	enc, dec := NewEncoder(conn), NewDecoder(conn)
	if err := enc.Encode(&Message{Type: TypeChallenge, Data: challenge}); err != nil {
		return nil, err
	}
	var m Message
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("protocol: read proof: %w", err)
	}
	if m.Type != TypeAuth || len(m.Challenge) != challengeSize || !hmac.Equal(m.Data, Proof(token, roleShim, challenge, m.PodUID)) {
		return nil, ErrUnauthenticated
	}
	if err := enc.Encode(&Message{Type: TypeAuth, Data: Proof(token, roleAgent, m.Challenge, string(challenge))}); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	c := newClient(conn, enc, dec)
	c.podUID = m.PodUID
	return c, nil
}

// Listener is the agent's end of the shim connection: a TCP listener the
// shim dials. Every connection must pass the mutual token handshake; only
// authenticated connections are handed out, and a newer one replaces the
// previous one.
type Listener struct {
	ln    net.Listener
	token []byte
	log   *slog.Logger

	ready chan *Client

	mu      sync.Mutex
	current *Client
	closed  chan struct{}
	once    sync.Once
}

// Listen listens on the TCP address addr (host:port) and starts accepting
// connections. With tlsConfig set, connections are TLS: the shim verifies the
// agent's certificate, and the token handshake runs inside the encrypted
// connection.
func Listen(addr string, token []byte, tlsConfig *tls.Config, log *slog.Logger) (*Listener, error) {
	if len(token) == 0 {
		return nil, errors.New("protocol: empty shim token")
	}
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	if tlsConfig != nil {
		// The TLS handshake runs on the first read of the token handshake,
		// under its deadline.
		ln = tls.NewListener(ln, tlsConfig)
	}
	l := &Listener{ln: ln, token: token, log: log, ready: make(chan *Client, 4), closed: make(chan struct{})}
	go l.acceptLoop()
	return l, nil
}

// Addr returns the address the listener is bound to.
func (l *Listener) Addr() string { return l.ln.Addr().String() }

func (l *Listener) acceptLoop() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.closed:
				return
			default:
			}
			// Transient (e.g. EMFILE): back off briefly and keep accepting.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go l.authenticate(conn)
	}
}

func (l *Listener) authenticate(conn net.Conn) {
	c, err := Challenge(conn, l.token, HandshakeTimeout)
	if err != nil {
		l.log.Warn("rejected shim connection", "remote", conn.RemoteAddr().String(), "error", err)
		_ = conn.Close()
		return
	}
	l.mu.Lock()
	prev := l.current
	l.current = c
	l.mu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
	select {
	case l.ready <- c:
	case <-l.closed:
		_ = c.Close()
	}
}

// Accept returns the next authenticated shim connection.
func (l *Listener) Accept(ctx context.Context) (*Client, error) {
	for {
		select {
		case c := <-l.ready:
			select {
			case <-c.Done():
				continue // replaced or dropped before it was picked up
			default:
				return c, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.closed:
			return nil, net.ErrClosed
		}
	}
}

// Close stops listening.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = l.ln.Close()
	})
	return err
}
