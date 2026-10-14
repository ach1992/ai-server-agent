package executor

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// This private bounded pipe is used only by the worker-owned, DAP-stdio
// consumer of broker #66. Protocol input never takes stdioSession.mu: the
// broker wait/stop path acquires d.mu after e.mu, so reading/writing through
// e.mu while the DAP wire holds d.mu could deadlock at adapter exit.
const dapStdioQueueLimit = 64 << 10

type dapStdoutQueue struct {
	mu      sync.Mutex
	data    []byte
	lost    bool
	done    bool
	exited  bool
	changed chan struct{}
}

func newDAPStdoutQueue() *dapStdoutQueue {
	return &dapStdoutQueue{changed: make(chan struct{})}
}

func (q *dapStdoutQueue) wakeLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func (q *dapStdoutQueue) wake() {
	q.mu.Lock()
	q.wakeLocked()
	q.mu.Unlock()
}

// os/exec's stdout copier cannot block indefinitely behind an unresponsive
// debugger. When the bounded queue is full, fail the DAP transport closed,
// rather than dropping bytes or inventing a valid partial protocol frame.
func (q *dapStdoutQueue) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done {
		return len(p), nil
	}
	if !q.lost {
		if len(p) > dapStdioQueueLimit-len(q.data) {
			q.lost = true
			q.data = nil
		} else {
			q.data = append(q.data, p...)
		}
		q.wakeLocked()
	}
	return len(p), nil
}

func (q *dapStdoutQueue) adapterExited() {
	q.mu.Lock()
	q.exited = true
	q.wakeLocked()
	q.mu.Unlock()
}

func (q *dapStdoutQueue) finish() {
	q.mu.Lock()
	q.done = true
	q.wakeLocked()
	q.mu.Unlock()
}

func (q *dapStdoutQueue) consume(p []byte) (int, error, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lost {
		return 0, errors.New("dap_stdio_output_lost"), nil
	}
	if len(q.data) > 0 {
		n := copy(p, q.data)
		q.data = q.data[n:]
		if len(q.data) == 0 {
			q.data = nil
		}
		return n, nil, nil
	}
	if q.done {
		return 0, io.EOF, nil
	}
	// The pinned adapter exited. If its stdout copier has not yet finished,
	// never claim clean termination from a speculative EOF or wait on e.mu.
	if q.exited {
		return 0, io.ErrUnexpectedEOF, nil
	}
	return 0, nil, q.changed
}

type stdioDAPConn struct {
	output        *dapStdoutQueue
	stdin         *os.File
	mu            sync.Mutex
	writeMu       sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
}

// Constructor is only called with an already authenticated, exact owner-
// bound broker entry. External callers cannot get a session's pipe handles.
func newStdioDAPConn(output *dapStdoutQueue, stdin *os.File) *stdioDAPConn {
	return &stdioDAPConn{output: output, stdin: stdin}
}

func (c *stdioDAPConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		c.mu.Lock()
		closed, deadline := c.closed, c.readDeadline
		c.mu.Unlock()
		if closed {
			return 0, net.ErrClosed
		}
		n, err, changed := c.output.consume(p)
		if n > 0 || err != nil {
			return n, err
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		if deadline.IsZero() {
			<-changed
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-changed:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (c *stdioDAPConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed, deadline := c.closed, c.writeDeadline
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if err := c.stdin.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	written := 0
	for written < len(p) {
		n := len(p) - written
		if n > maxStdioInputBytes {
			n = maxStdioInputBytes
		}
		m, err := c.stdin.Write(p[written : written+n])
		written += m
		if err != nil {
			return written, err
		}
		if m == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (c *stdioDAPConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	// The broker continues to own and close the actual stdin/process. Wake a
	// blocked DAP read without destroying the shared broker's process identity.
	c.output.wake()
	_ = c.stdin.SetWriteDeadline(time.Now())
	return nil
}

type dapStdioAddr string

func (a dapStdioAddr) Network() string       { return "worker-stdio" }
func (a dapStdioAddr) String() string        { return string(a) }
func (c *stdioDAPConn) LocalAddr() net.Addr  { return dapStdioAddr("executor-private") }
func (c *stdioDAPConn) RemoteAddr() net.Addr { return dapStdioAddr("pidfd-pinned-adapter") }
func (c *stdioDAPConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.readDeadline = t
	c.writeDeadline = t
	c.mu.Unlock()
	c.output.wake()
	return c.stdin.SetWriteDeadline(t)
}
func (c *stdioDAPConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.readDeadline = t
	c.mu.Unlock()
	c.output.wake()
	return nil
}
func (c *stdioDAPConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.writeDeadline = t
	c.mu.Unlock()
	return c.stdin.SetWriteDeadline(t)
}
