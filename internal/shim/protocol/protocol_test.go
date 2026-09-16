package protocol

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	now := time.Now().UTC().Truncate(time.Second)
	in := &Message{Type: TypeReply, ID: 7, OK: true, Status: &Status{Version: Version, Running: true, PID: 42, StartedAt: &now}}
	if err := enc.Encode(in); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(&Message{Type: TypeOutput, Data: []byte("hello\nworld")}); err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(&buf)
	var out Message
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Type != TypeReply || out.ID != 7 || !out.OK || out.Status == nil || out.Status.PID != 42 || !out.Status.StartedAt.Equal(now) {
		t.Fatalf("unexpected %+v", out)
	}
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Type != TypeOutput || string(out.Data) != "hello\nworld" {
		t.Fatalf("unexpected %+v", out)
	}
	if err := dec.Decode(&out); err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

// fakeShim answers status requests and emits one event.
func fakeShim(t *testing.T, conn net.Conn) {
	t.Helper()
	enc := NewEncoder(conn)
	dec := NewDecoder(conn)
	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		switch m.Type {
		case TypeStatus:
			_ = enc.Encode(&Message{Type: TypeReply, ID: m.ID, OK: true, Status: &Status{Version: Version}})
			_ = enc.Encode(&Message{Type: TypeStarted, PID: 5})
		case TypeKill:
			_ = enc.Encode(&Message{Type: TypeReply, ID: m.ID, OK: false, Error: "nothing running"})
		}
	}
}

func TestClientRequestAndEvents(t *testing.T) {
	a, b := net.Pipe()
	go fakeShim(t, b)
	c := NewClient(a)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil || st.Version != Version {
		t.Fatalf("status: %v %+v", err, st)
	}
	select {
	case ev := <-c.Events:
		if ev.Type != TypeStarted || ev.PID != 5 {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no event")
	}
	if err := c.Kill(ctx); err == nil {
		t.Fatal("expected error reply")
	}
	b.Close()
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("client did not observe close")
	}
}
