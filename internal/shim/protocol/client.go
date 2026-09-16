package protocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Client talks to a shim over its unix socket. One connection carries requests,
// replies and events. Events are delivered on the Events channel; the caller
// must drain it.
type Client struct {
	conn net.Conn
	enc  *Encoder
	dec  *Decoder

	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan *Message

	// Events receives every unsolicited message. It is closed when the connection ends.
	Events chan *Message

	closeOnce sync.Once
	closed    chan struct{}
	err       error
}

// Dial connects to the shim socket at path.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}

// NewClient wraps an established connection.
func NewClient(conn net.Conn) *Client {
	c := &Client{
		conn:    conn,
		enc:     NewEncoder(conn),
		dec:     NewDecoder(conn),
		pending: map[uint64]chan *Message{},
		Events:  make(chan *Message, 256),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.Events)
		close(c.closed)
	}()
	for {
		var m Message
		if err := c.dec.Decode(&m); err != nil {
			c.err = err
			return
		}
		if m.Type == TypeReply {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok {
				ch <- &m
			}
			continue
		}
		mm := m
		select {
		case c.Events <- &mm:
		default:
			// The consumer is not keeping up; drop output rather than block the shim.
			// Exited events must never be dropped.
			if mm.Type == TypeExited || mm.Type == TypeStarted {
				c.Events <- &mm
			}
		}
	}
}

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Err returns the error that ended the connection, if any.
func (c *Client) Err() error {
	select {
	case <-c.closed:
		if errors.Is(c.err, ErrClosed) {
			return nil
		}
		return c.err
	default:
		return nil
	}
}

// Close closes the connection.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	return err
}

// Request sends m and waits for the reply.
func (c *Client) Request(ctx context.Context, m *Message) (*Message, error) {
	m.ID = c.nextID.Add(1)
	ch := make(chan *Message, 1)
	c.mu.Lock()
	c.pending[m.ID] = ch
	c.mu.Unlock()
	if err := c.enc.Encode(m); err != nil {
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("protocol: connection closed while waiting for reply")
		}
		if !r.OK {
			return r, fmt.Errorf("shim: %s", r.Error)
		}
		return r, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("protocol: connection closed while waiting for reply")
	}
}

// Start asks the shim to spawn the process.
func (c *Client) Start(ctx context.Context, env []string) (*Status, error) {
	r, err := c.Request(ctx, &Message{Type: TypeStart, Env: env})
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}

// Stdin writes data to the process, chunked so that no line exceeds MaxLineSize.
func (c *Client) Stdin(ctx context.Context, data []byte) error {
	const chunk = 1 << 20
	for len(data) > 0 {
		n := min(len(data), chunk)
		if _, err := c.Request(ctx, &Message{Type: TypeStdin, Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Signal sends a signal (e.g. "SIGTERM") to the process group.
func (c *Client) Signal(ctx context.Context, sig string) error {
	_, err := c.Request(ctx, &Message{Type: TypeSignal, Signal: sig})
	return err
}

// Kill SIGKILLs the process group.
func (c *Client) Kill(ctx context.Context) error {
	_, err := c.Request(ctx, &Message{Type: TypeKill})
	return err
}

// Status reports the current process status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	r, err := c.Request(ctx, &Message{Type: TypeStatus})
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}

// Subscribe (re)subscribes to events; with replay the ring buffer is sent first as output events.
func (c *Client) Subscribe(ctx context.Context, replay bool) (*Status, error) {
	r, err := c.Request(ctx, &Message{Type: TypeSubscribe, Replay: replay})
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}

// WaitReady blocks until the socket accepts connections or ctx ends.
func WaitReady(ctx context.Context, path string, interval time.Duration) (*Client, error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c, err := Dial(ctx, path)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("shim socket %s not ready: %w (last error: %v)", path, ctx.Err(), err)
		case <-t.C:
		}
	}
}

// RemoteAddr returns the socket path the client is connected to.
func (c *Client) RemoteAddr() string { return c.conn.RemoteAddr().String() }
