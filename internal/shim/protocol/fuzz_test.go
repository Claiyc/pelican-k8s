package protocol

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// The shim decodes this stream from the agent socket and the agent decodes it
// from the shim; neither side may panic or read unbounded amounts of memory on
// a malformed line.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"type":"start","id":1,"env":["A=B"]}` + "\n"))
	f.Add([]byte(`{"type":"stdin","id":2,"data":"aGVsbG8="}` + "\n"))
	f.Add([]byte(`{"type":"exited","exit":{"code":1,"oomKilled":true,"at":"2026-01-01T00:00:00Z"}}` + "\n"))
	f.Add([]byte(`{"type":"stats","stats":{"memoryBytes":1,"cpuPercent":0.5}}` + "\n\n" + `{"type":"kill"}`))
	f.Add([]byte(`{"type":"start","id":3,"stop":{"type":"command","value":"stop"}}` + "\n"))
	f.Add([]byte(`{"type":"state","id":4,"value":"running"}` + "\n" + `{"type":"terminating"}` + "\n"))
	f.Add([]byte("{\n"))
	f.Add([]byte("\x00\xff not json\n"))

	f.Fuzz(func(t *testing.T, in []byte) {
		dec := NewDecoder(bytes.NewReader(in))
		for range 64 {
			var m Message
			if err := dec.Decode(&m); err != nil {
				return
			}
			// Anything that decoded must survive a relay: re-encoding it and
			// decoding that again has to produce the same bytes, so the agent
			// and the shim always agree on what a message means.
			var once bytes.Buffer
			if err := NewEncoder(&once).Encode(&m); err != nil {
				t.Fatalf("re-encode %+v: %v", m, err)
			}
			var back Message
			if err := NewDecoder(bytes.NewReader(once.Bytes())).Decode(&back); err != nil {
				t.Fatalf("re-decode %s: %v", once.Bytes(), err)
			}
			var twice bytes.Buffer
			if err := NewEncoder(&twice).Encode(&back); err != nil {
				t.Fatalf("re-encode %+v: %v", back, err)
			}
			if !bytes.Equal(once.Bytes(), twice.Bytes()) {
				t.Fatalf("relay changed the message:\n got %s\nwant %s", twice.Bytes(), once.Bytes())
			}
		}
	})
}

// The agent's shim port is reachable from the game pod, whose process does not
// hold the token. Whatever it sends during the handshake, on either side, it
// must neither crash the peer nor be authenticated.
func FuzzHandshake(f *testing.F) {
	f.Add([]byte(`{"type":"auth","data":"AAAA","challenge":"AAAA","podUID":"x"}` + "\n"))
	f.Add([]byte(`{"type":"challenge","data":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}` + "\n" + `{"type":"auth"}` + "\n"))
	f.Add([]byte(`{"type":"auth","data":null}` + "\n"))
	f.Add([]byte("garbage\n"))

	token := []byte("secret")
	f.Fuzz(func(t *testing.T, in []byte) {
		// The peer of the agent's Challenge.
		a, b := net.Pipe()
		go func() {
			_, _ = io.Copy(io.Discard, b)
		}()
		go func() {
			_, _ = b.Write(in)
		}()
		if c, err := Challenge(a, token, 100*time.Millisecond); err == nil {
			c.Close()
			t.Fatalf("authenticated %q", in)
		}
		a.Close()
		b.Close()

		// The peer of the shim's Answer.
		x, y := net.Pipe()
		go func() {
			_, _ = io.Copy(io.Discard, y)
		}()
		go func() {
			_, _ = y.Write(in)
		}()
		if _, err := Answer(x, NewEncoder(x), token, "pod", 100*time.Millisecond); err == nil {
			t.Fatalf("authenticated agent %q", in)
		}
		x.Close()
		y.Close()
	})
}
