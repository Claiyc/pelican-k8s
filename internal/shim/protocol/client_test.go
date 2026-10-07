package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedShim answers each request with the reply returned by handle (nil
// means no reply) and records the requests it saw.
type scriptedShim struct {
	mu   sync.Mutex
	seen []Message
}

func (s *scriptedShim) requests() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.seen...)
}

func serve(t *testing.T, handle func(m Message) *Message) (*Client, *scriptedShim, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	s := &scriptedShim{}
	go func() {
		enc, dec := NewEncoder(b), NewDecoder(b)
		for {
			var m Message
			if err := dec.Decode(&m); err != nil {
				return
			}
			s.mu.Lock()
			s.seen = append(s.seen, m)
			s.mu.Unlock()
			if r := handle(m); r != nil {
				r.Type, r.ID = TypeReply, m.ID
				_ = enc.Encode(r)
			}
		}
	}()
	c := NewClient(a)
	t.Cleanup(func() { c.Close(); b.Close() })
	return c, s, b
}

func TestClientCommands(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Second)
	c, shim, _ := serve(t, func(m Message) *Message {
		switch m.Type {
		case TypeStart, TypeStatus, TypeSubscribe:
			return &Message{OK: true, Status: &Status{Version: Version, Running: m.Type == TypeStart, PID: 9, StartedAt: &started}}
		}
		return &Message{OK: true}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := c.Start(ctx, []string{"A=1", "B=2"}, &StopConfig{Type: StopCommand, Value: "stop"})
	if err != nil || !st.Running || st.PID != 9 {
		t.Fatalf("Start: %v %+v", err, st)
	}
	if err := c.Signal(ctx, "SIGTERM"); err != nil {
		t.Fatal(err)
	}
	if err := c.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Subscribe(ctx, true); err != nil || st.Running || !st.StartedAt.Equal(started) {
		t.Fatalf("Subscribe: %v %+v", err, st)
	}
	if err := c.Configure(ctx, StopConfig{Type: StopSignal, Value: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	if err := c.State(ctx, "running"); err != nil {
		t.Fatal(err)
	}

	reqs := shim.requests()
	if len(reqs) != 6 {
		t.Fatalf("requests: %+v", reqs)
	}
	if reqs[0].Type != TypeStart || strings.Join(reqs[0].Env, ",") != "A=1,B=2" || reqs[0].Stop == nil || *reqs[0].Stop != (StopConfig{Type: StopCommand, Value: "stop"}) {
		t.Errorf("start request: %+v", reqs[0])
	}
	if reqs[4].Type != TypeConfigure || reqs[4].Stop == nil || *reqs[4].Stop != (StopConfig{Type: StopSignal, Value: "SIGINT"}) {
		t.Errorf("configure request: %+v", reqs[4])
	}
	if reqs[5].Type != TypeState || reqs[5].Value != "running" {
		t.Errorf("state request: %+v", reqs[5])
	}
	if reqs[1].Type != TypeSignal || reqs[1].Signal != "SIGTERM" {
		t.Errorf("signal request: %+v", reqs[1])
	}
	if reqs[2].Type != TypeKill {
		t.Errorf("kill request: %+v", reqs[2])
	}
	if reqs[3].Type != TypeSubscribe || !reqs[3].Replay {
		t.Errorf("subscribe request: %+v", reqs[3])
	}
	// Request ids are unique and increasing so replies cannot be mixed up.
	for i := 1; i < len(reqs); i++ {
		if reqs[i].ID <= reqs[i-1].ID {
			t.Errorf("ids not increasing: %d then %d", reqs[i-1].ID, reqs[i].ID)
		}
	}
}

func TestClientStdinChunksLargePayloads(t *testing.T) {
	c, shim, _ := serve(t, func(m Message) *Message { return &Message{OK: true} })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload := bytes.Repeat([]byte("x"), 2*1024*1024+17)
	if err := c.Stdin(ctx, payload); err != nil {
		t.Fatal(err)
	}
	var total int
	reqs := shim.requests()
	for _, r := range reqs {
		if r.Type != TypeStdin {
			t.Fatalf("unexpected request %v", r.Type)
		}
		total += len(r.Data)
	}
	if len(reqs) != 3 || total != len(payload) {
		t.Fatalf("%d chunks carrying %d bytes, want 3 chunks carrying %d", len(reqs), total, len(payload))
	}

	if err := c.Stdin(ctx, nil); err != nil || len(shim.requests()) != 3 {
		t.Fatalf("empty stdin must send nothing: %v", err)
	}
}

func TestClientStdinStopsAtTheFirstError(t *testing.T) {
	c, shim, _ := serve(t, func(m Message) *Message { return &Message{OK: false, Error: "process not running"} })
	err := c.Stdin(context.Background(), bytes.Repeat([]byte("x"), 3*1024*1024))
	if err == nil || !strings.Contains(err.Error(), "shim: process not running") {
		t.Fatalf("err = %v", err)
	}
	if n := len(shim.requests()); n != 1 {
		t.Fatalf("%d chunks sent after a failure", n)
	}
}

func TestClientRequestFailures(t *testing.T) {
	t.Run("error reply is returned with its message", func(t *testing.T) {
		c, _, _ := serve(t, func(m Message) *Message { return &Message{OK: false, Error: "boom"} })
		r, err := c.Request(context.Background(), &Message{Type: TypeStatus})
		if err == nil || err.Error() != "shim: boom" || r == nil || r.Error != "boom" {
			t.Fatalf("r=%+v err=%v", r, err)
		}
		if _, err := c.Status(context.Background()); err == nil {
			t.Fatal("Status must propagate the error")
		}
		if _, err := c.Start(context.Background(), nil, nil); err == nil {
			t.Fatal("Start must propagate the error")
		}
		if _, err := c.Subscribe(context.Background(), false); err == nil {
			t.Fatal("Subscribe must propagate the error")
		}
		if err := c.Signal(context.Background(), "SIGINT"); err == nil {
			t.Fatal("Signal must propagate the error")
		}
	})

	t.Run("context cancellation drops the pending request", func(t *testing.T) {
		c, _, _ := serve(t, func(m Message) *Message { return nil })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := c.Request(ctx, &Message{Type: TypeStatus}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d pending requests leaked", n)
		}
	})

	t.Run("connection closing while waiting", func(t *testing.T) {
		c, _, peer := serve(t, func(m Message) *Message { return nil })
		go func() { time.Sleep(50 * time.Millisecond); peer.Close() }()
		_, err := c.Request(context.Background(), &Message{Type: TypeStatus})
		if err == nil || !strings.Contains(err.Error(), "connection closed") {
			t.Fatalf("err = %v", err)
		}
		if err := c.Err(); err != nil {
			t.Fatalf("a clean EOF is not an error, got %v", err)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		c, _, _ := serve(t, func(m Message) *Message { return nil })
		c.Close()
		if _, err := c.Request(context.Background(), &Message{Type: TypeStatus}); err == nil {
			t.Fatal("request on a closed connection must fail")
		}
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d pending requests leaked", n)
		}
	})

	t.Run("reply with an unknown id is ignored", func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		c := NewClient(a)
		defer c.Close()
		go func() {
			enc, dec := NewEncoder(b), NewDecoder(b)
			var m Message
			if dec.Decode(&m) != nil {
				return
			}
			_ = enc.Encode(&Message{Type: TypeReply, ID: m.ID + 100, OK: true})
			_ = enc.Encode(&Message{Type: TypeReply, ID: m.ID, OK: true, Status: &Status{PID: 3}})
		}()
		st, err := c.Status(context.Background())
		if err != nil || st.PID != 3 {
			t.Fatalf("st=%+v err=%v", st, err)
		}
	})
}

func TestClientErrReportsAbnormalEnd(t *testing.T) {
	a, b := net.Pipe()
	c := NewClient(a)
	defer c.Close()
	if c.Err() != nil {
		t.Fatal("Err before the end must be nil")
	}
	_, _ = b.Write([]byte("this is not json\n"))
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not stop on a malformed message")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Err = %v", err)
	}
	b.Close()
	if _, ok := <-c.Events; ok {
		t.Fatal("Events must be closed")
	}
}

// A consumer that does not drain Events loses output but never the process
// lifecycle events.
func TestClientDropsOutputButKeepsExit(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a)
	defer c.Close()
	enc := NewEncoder(b)

	const sent = 300
	for i := 0; i < sent; i++ {
		if err := enc.Encode(&Message{Type: TypeOutput, Data: []byte("line")}); err != nil {
			t.Fatal(err)
		}
	}
	exited := make(chan struct{})
	go func() {
		_ = enc.Encode(&Message{Type: TypeExited, Exit: &ExitState{Code: 3}})
		close(exited)
	}()

	// Drain: the first 256 outputs were queued, the rest dropped, and the exit
	// event follows once there is room.
	var outputs int
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-c.Events:
			if ev.Type == TypeExited {
				if ev.Exit == nil || ev.Exit.Code != 3 {
					t.Fatalf("exit event %+v", ev)
				}
				if outputs == 0 || outputs > sent {
					t.Fatalf("%d outputs delivered", outputs)
				}
				<-exited
				return
			}
			outputs++
		case <-deadline:
			t.Fatalf("exit event never delivered after %d outputs", outputs)
		}
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestEncodeWriteError(t *testing.T) {
	want := errors.New("disk full")
	if err := NewEncoder(failingWriter{want}).Encode(&Message{Type: TypeStatus}); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeSkipsBlankLinesAndRejectsGarbage(t *testing.T) {
	dec := NewDecoder(strings.NewReader("\n\n{\"type\":\"status\"}\n{oops\n"))
	var m Message
	if err := dec.Decode(&m); err != nil || m.Type != TypeStatus {
		t.Fatalf("m=%+v err=%v", m, err)
	}
	if err := dec.Decode(&m); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeOversizedLine(t *testing.T) {
	line := strings.Repeat("a", MaxLineSize+10)
	dec := NewDecoder(io.MultiReader(strings.NewReader(line), strings.NewReader("\n")))
	var m Message
	if err := dec.Decode(&m); err == nil || errors.Is(err, ErrClosed) {
		t.Fatalf("an oversized line must be an error, got %v", err)
	}
}

func TestHandshakeEdgeCases(t *testing.T) {
	if _, err := Listen("127.0.0.1:0", nil, nil); err == nil {
		t.Fatal("Listen without a token")
	}
	if _, err := Listen("256.0.0.1:0", []byte("t"), nil); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Listen on an invalid address: %v", err)
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := Challenge(a, nil, time.Second); err == nil {
		t.Fatal("Challenge without a token")
	}
	if _, err := Answer(a, NewEncoder(a), nil, "pod", time.Second); err == nil {
		t.Fatal("Answer without a token")
	}

	// The peer closes before reading the challenge, and before answering.
	c1, c2 := net.Pipe()
	c2.Close()
	if _, err := Challenge(c1, []byte("t"), time.Second); err == nil {
		t.Fatal("Challenge against a closed peer")
	}
	d1, d2 := net.Pipe()
	d2.Close()
	if _, err := Answer(d1, NewEncoder(d1), []byte("t"), "pod", time.Second); err == nil || !strings.Contains(err.Error(), "read challenge") {
		t.Fatalf("Answer against a closed peer: %v", err)
	}

	// A peer that answers with the wrong message type is rejected.
	e1, e2 := net.Pipe()
	defer e2.Close()
	go func() {
		dec := NewDecoder(e2)
		var m Message
		_ = dec.Decode(&m)
		_ = NewEncoder(e2).Encode(&Message{Type: TypeStatus, Data: Proof([]byte("t"), roleShim, m.Data, ""), Challenge: make([]byte, challengeSize)})
	}()
	if _, err := Challenge(e1, []byte("t"), time.Second); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong message type: %v", err)
	}

	// A shim proof without a challenge of its own is rejected.
	f1, f2 := net.Pipe()
	defer f2.Close()
	go func() {
		dec := NewDecoder(f2)
		var m Message
		_ = dec.Decode(&m)
		_ = NewEncoder(f2).Encode(&Message{Type: TypeAuth, Data: Proof([]byte("t"), roleShim, m.Data, "")})
	}()
	if _, err := Challenge(f1, []byte("t"), time.Second); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("missing shim challenge: %v", err)
	}
}

func TestListenerLifecycle(t *testing.T) {
	ln, err := Listen("127.0.0.1:0", []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ln.Addr(), "127.0.0.1:") {
		t.Fatalf("Addr = %q", ln.Addr())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ln.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Accept with nobody connecting: %v", err)
	}

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := ln.Accept(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v", err)
	}
	if _, err := net.Dial("tcp", ln.Addr()); err == nil {
		t.Fatal("the listener still accepts connections after Close")
	}
}

// A connection that was replaced before the agent picked it up is skipped.
func TestAcceptSkipsDeadConnections(t *testing.T) {
	ln := listen(t, "secret")
	token := []byte("secret")

	first, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Answer(first, NewEncoder(first), token, "pod-1", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondDec, err := Answer(second, NewEncoder(second), token, "pod-2", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the newer connection replaced the older one.
	deadline := time.Now().Add(3 * time.Second)
	for {
		ln.mu.Lock()
		n := len(ln.ready)
		ln.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ready queue length = %d, want 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Answer the first request on the newer connection only.
	go func() {
		var m Message
		if secondDec.Decode(&m) != nil {
			return
		}
		_ = NewEncoder(second).Encode(&Message{Type: TypeReply, ID: m.ID, OK: true, Status: &Status{PID: 7}})
	}()
	c, err := accept(t, ln, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil || st.PID != 7 {
		t.Fatalf("Accept returned the replaced connection: status %+v, err %v", st, err)
	}
	first.Close()
}
