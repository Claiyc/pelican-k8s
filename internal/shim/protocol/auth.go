package protocol

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// TokenEnv names the environment variable that carries the shared shim token
// into the agent and the shim. The shim removes it from the environment of the
// game process and makes itself non-dumpable, so the game cannot read it.
const TokenEnv = "PELICAN_SHIM_TOKEN"

// Handshake message types. The agent opens every connection with a challenge;
// the shim answers with a proof of the shared token.
const (
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
)

// HandshakeTimeout bounds the authentication of a new connection.
const HandshakeTimeout = 5 * time.Second

const challengeSize = 32

// Proof is the shim's answer to a challenge: HMAC-SHA256 over the challenge
// with the shared token as key.
func Proof(token, challenge []byte) []byte {
	m := hmac.New(sha256.New, token)
	m.Write([]byte("pelican-k8s shim auth v1\x00"))
	m.Write(challenge)
	return m.Sum(nil)
}

// ErrUnauthenticated is returned when a peer fails the handshake.
var ErrUnauthenticated = errors.New("protocol: peer failed authentication")

// Answer runs the shim side of the handshake on a connection the shim dialed:
// it reads the agent's challenge and replies with the proof. The returned
// decoder must be used for the rest of the connection, since it may already
// hold buffered bytes.
func Answer(conn net.Conn, enc *Encoder, token []byte, timeout time.Duration) (*Decoder, error) {
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
	if err := enc.Encode(&Message{Type: TypeAuth, Data: Proof(token, m.Data)}); err != nil {
		return nil, err
	}
	return dec, nil
}

// Challenge runs the agent side of the handshake on an accepted connection and
// returns a Client once the peer has proven knowledge of the token.
func Challenge(conn net.Conn, token []byte, timeout time.Duration) (*Client, error) {
	if len(token) == 0 {
		return nil, errors.New("protocol: empty shim token")
	}
	challenge := make([]byte, challengeSize)
	if _, err := rand.Read(challenge); err != nil {
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
	if m.Type != TypeAuth || !hmac.Equal(m.Data, Proof(token, challenge)) {
		return nil, ErrUnauthenticated
	}
	_ = conn.SetDeadline(time.Time{})
	return newClient(conn, enc, dec), nil
}

// Listener is the agent's end of the shim socket. The agent listens and the
// shim connects, so the socket lives in a directory that only the agent
// container can write: the game container mounts it read-only. Every
// connection must pass the token handshake; only authenticated connections
// are handed out, and a newer one replaces the previous one.
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

// Listen creates the socket at path (replacing a stale one) with mode 0600
// and starts accepting connections.
func Listen(path string, token []byte, log *slog.Logger) (*Listener, error) {
	if len(token) == 0 {
		return nil, errors.New("protocol: empty shim token")
	}
	if log == nil {
		log = slog.Default()
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	l := &Listener{ln: ln, token: token, log: log, ready: make(chan *Client, 4), closed: make(chan struct{})}
	go l.acceptLoop()
	return l, nil
}

// Path returns the socket path.
func (l *Listener) Path() string { return l.ln.Addr().String() }

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
		l.log.Warn("rejected shim socket connection", "error", err)
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

// Close stops listening and removes the socket.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = l.ln.Close()
	})
	return err
}
