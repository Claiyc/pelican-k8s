package ringbuf

import (
	"bytes"
	"testing"
)

// The ring buffer absorbs arbitrary game-server output. Whatever the sequence
// of writes, it must retain exactly the last cap bytes and never index out of
// range.
func FuzzWrite(f *testing.F) {
	f.Add(8, []byte("hello"), []byte("world"))
	f.Add(1, []byte(""), []byte("x"))
	f.Add(4, []byte("abcdefgh"), []byte("ij"))
	f.Add(0, []byte("a"), []byte("bc"))

	f.Fuzz(func(t *testing.T, size int, a, b []byte) {
		if size < -1024 || size > 1<<16 {
			t.Skip()
		}
		buf := New(size)
		buf.Write(a)
		buf.Write(b)

		want := append(append([]byte(nil), a...), b...)
		if len(want) > buf.cap() {
			want = want[len(want)-buf.cap():]
		}
		got := buf.Bytes()
		if !bytes.Equal(got, want) {
			t.Fatalf("size %d: got %q, want %q", size, got, want)
		}
		if buf.Len() != len(want) {
			t.Fatalf("size %d: Len %d, want %d", size, buf.Len(), len(want))
		}
		buf.Reset()
		if buf.Len() != 0 || len(buf.Bytes()) != 0 {
			t.Fatalf("not empty after Reset: %q", buf.Bytes())
		}
	})
}

// cap is the buffer's fixed capacity, which New clamps to at least 1.
func (b *Buffer) cap() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}
