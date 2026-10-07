package protocol

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// dialAndAnswer plays the shim: connect and run the shim side of the handshake.
func dialAndAnswer(t *testing.T, addr string, token []byte) (net.Conn, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = Answer(conn, NewEncoder(conn), token, "pod-1", 2*time.Second)
	return conn, err
}

func listen(t *testing.T, token string) *Listener {
	t.Helper()
	ln, err := Listen("127.0.0.1:0", []byte(token), nil)
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
	conn, err := dialAndAnswer(t, ln.Addr(), []byte("secret"))
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
	conn, err := dialAndAnswer(t, ln.Addr(), []byte("guess"))
	if err == nil {
		t.Fatal("the shim side completed a handshake the agent rejected")
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
		silent, err := net.Dial("tcp", ln.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer silent.Close()
	}
	conn, err := dialAndAnswer(t, ln.Addr(), []byte("secret"))
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
	first, err := dialAndAnswer(t, ln.Addr(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	c1, err := accept(t, ln, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dialAndAnswer(t, ln.Addr(), []byte("secret"))
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
	if _, err := Answer(a, NewEncoder(a), []byte("secret"), "pod", time.Second); err == nil {
		t.Fatal("short challenge answered")
	}
}

// The shim learns that the agent holds the token too: a listener with the
// wrong token (anything that is not the agent) gets no commands through.
func TestShimRejectsAgentWithoutToken(t *testing.T) {
	ln := listen(t, "not-the-token")
	if _, err := dialAndAnswer(t, ln.Addr(), []byte("secret")); err == nil {
		t.Fatal("handshake with an agent that does not hold the token succeeded")
	}
}

// A peer that replays the shim's own proof as the agent's is rejected: the
// proofs are bound to the role that computed them.
func TestShimRejectsReflectedProof(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	token := []byte("secret")
	go func() {
		enc, dec := NewEncoder(b), NewDecoder(b)
		challenge := make([]byte, challengeSize)
		_ = enc.Encode(&Message{Type: TypeChallenge, Data: challenge})
		var m Message
		if dec.Decode(&m) != nil {
			return
		}
		_ = enc.Encode(&Message{Type: TypeAuth, Data: m.Data})
	}()
	if _, err := Answer(a, NewEncoder(a), token, "pod", time.Second); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("reflected proof: %v", err)
	}
}

// The agent learns the shim's pod UID from the handshake, and the pod UID is
// covered by the proof.
func TestHandshakeCarriesPodUID(t *testing.T) {
	ln := listen(t, "secret")
	conn, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Answer(conn, NewEncoder(conn), []byte("secret"), "8c1f-uid", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	c, err := accept(t, ln, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.PodUID() != "8c1f-uid" {
		t.Fatalf("PodUID = %q", c.PodUID())
	}

	// A proof computed for one pod UID does not hold for another.
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	go func() {
		enc, dec := NewEncoder(y), NewDecoder(y)
		var m Message
		if dec.Decode(&m) != nil {
			return
		}
		_ = enc.Encode(&Message{Type: TypeAuth, Data: Proof([]byte("secret"), roleShim, m.Data, "pod-a"), Challenge: make([]byte, challengeSize), PodUID: "pod-b"})
	}()
	if _, err := Challenge(x, []byte("secret"), time.Second); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pod UID swapped after the proof: %v", err)
	}
}
