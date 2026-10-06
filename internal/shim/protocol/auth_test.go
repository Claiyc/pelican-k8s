package protocol

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// dialAndAnswer plays the shim: connect, answer the challenge with token.
func dialAndAnswer(t *testing.T, path string, token []byte) (net.Conn, error) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Answer(conn, NewEncoder(conn), token, 2*time.Second)
	return conn, err
}

func listen(t *testing.T, token string) *Listener {
	t.Helper()
	ln, err := Listen(filepath.Join(t.TempDir(), "shim.sock"), []byte(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func accept(t *testing.T, ln *Listener, within time.Duration) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return ln.Accept(ctx)
}

func TestListenerAcceptsTheTokenHolder(t *testing.T) {
	ln := listen(t, "secret")
	conn, err := dialAndAnswer(t, ln.Path(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, err := accept(t, ln, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The connection carries the protocol after the handshake.
	go func() {
		dec := NewDecoder(conn)
		var m Message
		if dec.Decode(&m) == nil {
			_ = NewEncoder(conn).Encode(&Message{Type: TypeReply, ID: m.ID, OK: true, Status: &Status{Version: Version}})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if st, err := c.Status(ctx); err != nil || st.Version != Version {
		t.Fatalf("status after handshake: %+v %v", st, err)
	}
}

// A process that does not hold the token (the game, which shares the UID and
// can connect to the socket) is rejected and never handed to the agent.
func TestListenerRejectsWrongToken(t *testing.T) {
	ln := listen(t, "secret")
	conn, err := dialAndAnswer(t, ln.Path(), []byte("guess"))
	if err != nil {
		t.Fatal(err) // the shim side cannot tell; the agent closes the connection
	}
	defer conn.Close()
	if c, err := accept(t, ln, 300*time.Millisecond); err == nil {
		t.Fatalf("connection with a wrong token accepted: %v", c)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("rejected connection left open")
	}
}

// A peer that connects and stays silent must not delay the real shim.
func TestListenerSilentPeerDoesNotBlock(t *testing.T) {
	ln := listen(t, "secret")
	for range 3 {
		silent, err := net.Dial("unix", ln.Path())
		if err != nil {
			t.Fatal(err)
		}
		defer silent.Close()
	}
	conn, err := dialAndAnswer(t, ln.Path(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := accept(t, ln, time.Second); err != nil {
		t.Fatalf("real shim not accepted while silent peers are pending: %v", err)
	}
}

// A newer authenticated connection (a restarted shim) replaces the older one.
func TestListenerNewestWins(t *testing.T) {
	ln := listen(t, "secret")
	first, err := dialAndAnswer(t, ln.Path(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	c1, err := accept(t, ln, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dialAndAnswer(t, ln.Path(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	c2, err := accept(t, ln, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c1.Done():
	case <-time.After(time.Second):
		t.Fatal("previous connection not closed")
	}
	select {
	case <-c2.Done():
		t.Fatal("newest connection closed")
	default:
	}
}

// The shim answers only a well-formed challenge.
func TestAnswerRejectsMalformedChallenge(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = NewEncoder(b).Encode(&Message{Type: TypeChallenge, Data: []byte("short")}) }()
	if _, err := Answer(a, NewEncoder(a), []byte("secret"), time.Second); err == nil {
		t.Fatal("short challenge answered")
	}
}
