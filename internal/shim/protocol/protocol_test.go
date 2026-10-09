package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
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
	if err := dec.Decode(&out); !errors.Is(err, ErrClosed) {
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

// stalledReader never has data and never ends.
type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }

func TestDecoderLimit(t *testing.T) {
	long := `{"type":"output","data":"` + strings.Repeat("A", 8000) + `"}` + "\n"
	d := NewDecoder(strings.NewReader(long))
	d.limit = 4096
	if err := d.Decode(&Message{}); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("over the limit: %v, want ErrTooLong", err)
	}
	if err := NewDecoder(strings.NewReader(long)).Decode(&Message{}); err != nil {
		t.Fatalf("within MaxLineSize: %v", err)
	}
	head, tail := `{"type":"output"`, `}`
	for _, eol := range []string{"\n", "\r\n"} {
		full := head + strings.Repeat(" ", MaxLineSize-len(head)-len(tail)) + tail + eol
		if err := NewDecoder(strings.NewReader(full)).Decode(&Message{}); err != nil {
			t.Fatalf("line of MaxLineSize ending in %q: %v", eol, err)
		}
	}
	// An unfinished line one byte past the limit is cut unless that byte is
	// the "\r" of its ending.
	d = NewDecoder(io.MultiReader(strings.NewReader(strings.Repeat("A", 4097)), stalledReader{}))
	d.limit = 4096
	if err := d.Decode(&Message{}); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("unfinished line of limit+1: %v, want ErrTooLong", err)
	}
	over := head + strings.Repeat(" ", MaxLineSize+1-len(head)-len(tail)) + tail + "\n"
	if err := NewDecoder(strings.NewReader(over)).Decode(&Message{}); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("line of MaxLineSize+1: %v, want ErrTooLong", err)
	}
}
