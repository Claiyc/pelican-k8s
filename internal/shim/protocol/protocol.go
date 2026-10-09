// Package protocol defines the newline-delimited JSON protocol spoken between
// the agent and the shim over TCP. The agent listens and sends requests; the
// shim dials, both sides prove knowledge of the shared token (auth.go), and
// the shim answers the requests.
//
// Requests carry an ID; the shim answers every request with a Reply of the same
// ID. Events (output, stats, started, exited, terminating) are pushed to every
// subscribed connection without a request.
package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Message types.
const (
	TypeStart     = "start"     // request: start the process with the given environment and stop configuration
	TypeConfigure = "configure" // request: replace the stop configuration
	TypeState     = "state"     // request: the agent's process state (drives the readiness file)
	TypeStdin     = "stdin"     // request: write bytes to the process stdin
	TypeSignal    = "signal"    // request: send a signal to the process group
	TypeKill      = "kill"      // request: SIGKILL the process group
	TypeStatus    = "status"    // request: report the current status
	TypeSubscribe = "subscribe" // request: (re)subscribe to events, optionally replaying the ring buffer
	TypeReply     = "reply"     // response to a request
	TypeOutput    = "output"    // event: PTY bytes
	TypeStarted   = "started"   // event: process spawned
	TypeExited    = "exited"    // event: process exited
	TypeStats     = "stats"     // event: resource usage sample
	// TypeTerminating is the event sent when the shim received SIGTERM and
	// stops the process by itself.
	TypeTerminating = "terminating"
)

// Version of the protocol; bumped on incompatible changes.
const Version = 2

// Stop types of a StopConfig, as in Wings' process configuration.
const (
	StopCommand = "command"
	StopSignal  = "signal"
)

// StopConfig is Wings' stop configuration: a command written to stdin, or a
// signal sent to the process group. The shim keeps it to stop the process by
// itself when its container is terminated.
type StopConfig struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Message is the wire format for both directions.
type Message struct {
	Type string `json:"type"`
	// ID correlates requests and replies.
	ID uint64 `json:"id,omitempty"`

	// Request fields.
	Env    []string    `json:"env,omitempty"`    // start
	Stop   *StopConfig `json:"stop,omitempty"`   // start, configure
	Value  string      `json:"value,omitempty"`  // state
	Data   []byte      `json:"data,omitempty"`   // stdin, output (base64 in JSON)
	Signal string      `json:"signal,omitempty"` // signal
	Replay bool        `json:"replay,omitempty"` // subscribe

	// Handshake fields.
	Challenge []byte `json:"challenge,omitempty"`
	PodUID    string `json:"podUID,omitempty"`

	// Reply fields.
	OK     bool    `json:"ok,omitempty"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`

	// Event fields.
	Exit  *ExitState `json:"exit,omitempty"`
	Stats *Stats     `json:"stats,omitempty"`
	PID   int        `json:"pid,omitempty"`
}

// Status describes the supervised process.
type Status struct {
	Version   int        `json:"version"`
	Running   bool       `json:"running"`
	PID       int        `json:"pid,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	LastExit  *ExitState `json:"lastExit,omitempty"`
	// Stopping is set once the shim itself received SIGTERM.
	Stopping bool `json:"stopping,omitempty"`
	// PodUID is the UID of the shim's pod.
	PodUID string `json:"podUID,omitempty"`
}

// ExitState is the exit of the supervised process.
type ExitState struct {
	Code      int       `json:"code"`
	OOMKilled bool      `json:"oomKilled"`
	Signal    string    `json:"signal,omitempty"`
	At        time.Time `json:"at"`
}

// Stats is one resource usage sample of the container cgroup.
type Stats struct {
	MemoryBytes      uint64 `json:"memoryBytes"`
	MemoryLimitBytes uint64 `json:"memoryLimitBytes"`
	// CPUPercent is the usage since the previous sample as percent of one core.
	CPUPercent    float64 `json:"cpuPercent"`
	NetworkRx     uint64  `json:"networkRx"`
	NetworkTx     uint64  `json:"networkTx"`
	DiskReadBytes uint64  `json:"diskReadBytes"`
	DiskWrite     uint64  `json:"diskWriteBytes"`
	UptimeMillis  int64   `json:"uptimeMillis"`
}

// Encoder writes messages as JSON lines and is safe for concurrent use.
type Encoder struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  interface{ Flush() error }
}

// NewEncoder wraps w.
func NewEncoder(w io.Writer) *Encoder {
	bw := bufio.NewWriterSize(w, 64*1024)
	return &Encoder{w: bw, f: bw}
}

// Encode writes one message followed by a newline.
func (e *Encoder) Encode(m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return err
	}
	return e.w.Flush()
}

// MaxLineSize bounds a single protocol line (stdin payloads are chunked by the client).
const MaxLineSize = 4 * 1024 * 1024

// Decoder reads JSON lines.
type Decoder struct {
	s *bufio.Scanner
	// limit bounds a line; it starts at MaxLineSize and is lowered while a
	// connection is not yet authenticated.
	limit int
}

// NewDecoder wraps r.
func NewDecoder(r io.Reader) *Decoder {
	d := &Decoder{s: bufio.NewScanner(r), limit: MaxLineSize}
	// The buffer also holds the line's "\r\n", which the token leaves out.
	d.s.Buffer(make([]byte, 0, 64*1024), MaxLineSize+2)
	d.s.Split(d.split)
	return d
}

// split is bufio.ScanLines that fails once a line grows past the limit. An
// unfinished line may hold one byte more, a "\r" waiting for its "\n".
func (d *Decoder) split(data []byte, atEOF bool) (int, []byte, error) {
	advance, token, err := bufio.ScanLines(data, atEOF)
	if err == nil && (len(token) > d.limit || token == nil && pastLimit(data, d.limit)) {
		return 0, nil, bufio.ErrTooLong
	}
	return advance, token, err
}

// pastLimit reports an unfinished line that can no longer end within limit.
func pastLimit(data []byte, limit int) bool {
	return len(data) > limit+1 || len(data) == limit+1 && data[limit] != '\r'
}

// ErrClosed is returned by Decode when the stream ended.
var ErrClosed = errors.New("protocol: stream closed")

// Decode reads the next message.
func (d *Decoder) Decode(m *Message) error {
	for d.s.Scan() {
		line := d.s.Bytes()
		if len(line) == 0 {
			continue
		}
		*m = Message{}
		if err := json.Unmarshal(line, m); err != nil {
			return fmt.Errorf("protocol: malformed message: %w", err)
		}
		return nil
	}
	if err := d.s.Err(); err != nil {
		return err
	}
	return ErrClosed
}
