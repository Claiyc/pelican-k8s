package ringbuf

import (
	"bytes"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	b := New(8)
	if got := b.Bytes(); len(got) != 0 {
		t.Fatalf("empty buffer returned %q", got)
	}
	b.Write([]byte("abc"))
	if got := b.Bytes(); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("got %q", got)
	}
	b.Write([]byte("defgh")) // exactly fills
	if got := b.Bytes(); !bytes.Equal(got, []byte("abcdefgh")) {
		t.Fatalf("got %q", got)
	}
	b.Write([]byte("ij")) // wraps
	if got := b.Bytes(); !bytes.Equal(got, []byte("cdefghij")) {
		t.Fatalf("got %q", got)
	}
	b.Write([]byte("0123456789ABCDEF")) // larger than capacity
	if got := b.Bytes(); !bytes.Equal(got, []byte("89ABCDEF")) {
		t.Fatalf("got %q", got)
	}
	if b.Len() != 8 {
		t.Fatalf("len %d", b.Len())
	}
	b.Reset()
	if b.Len() != 0 {
		t.Fatalf("len after reset %d", b.Len())
	}
	b.Write([]byte("xy"))
	if got := b.Bytes(); !bytes.Equal(got, []byte("xy")) {
		t.Fatalf("got %q", got)
	}
}
