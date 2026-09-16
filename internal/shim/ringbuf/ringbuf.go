// Package ringbuf is a fixed-size byte ring buffer keeping the most recent bytes written.
package ringbuf

import "sync"

// Buffer keeps the last N bytes written to it.
type Buffer struct {
	mu   sync.Mutex
	buf  []byte
	w    int  // next write position
	full bool // wrapped at least once
}

// New returns a buffer of the given capacity.
func New(size int) *Buffer {
	if size <= 0 {
		size = 1
	}
	return &Buffer{buf: make([]byte, size)}
}

// Write implements io.Writer and never fails.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= len(b.buf) {
		copy(b.buf, p[n-len(b.buf):])
		b.w = 0
		b.full = true
		return n, nil
	}
	first := copy(b.buf[b.w:], p)
	if first < n {
		copy(b.buf, p[first:])
		b.full = true
	}
	b.w = (b.w + n) % len(b.buf)
	if b.w == 0 && n > 0 {
		b.full = true
	}
	return n, nil
}

// Bytes returns a copy of the retained bytes in order.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return append([]byte(nil), b.buf[:b.w]...)
	}
	out := make([]byte, 0, len(b.buf))
	out = append(out, b.buf[b.w:]...)
	out = append(out, b.buf[:b.w]...)
	return out
}

// Reset discards all content.
func (b *Buffer) Reset() {
	b.mu.Lock()
	b.w = 0
	b.full = false
	b.mu.Unlock()
}

// Len is the number of retained bytes.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return len(b.buf)
	}
	return b.w
}
