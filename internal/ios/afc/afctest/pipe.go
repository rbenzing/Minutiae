package afctest

import (
	"io"
	"sync"
)

// half is one direction of an in-memory connection. Unlike net.Pipe it
// buffers: a write never waits for the reader, as on a socket, so a device
// whose reply runs on past one packet does not stall in its write.
type half struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	wclosed bool // the writer is done: the reader drains the buffer, then sees EOF
	rclosed bool // the reader is gone: reads fail at once, writes fail
}

func newHalf() *half {
	h := &half{}
	h.cond = sync.NewCond(&h.mu)
	return h
}

func (h *half) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.wclosed || h.rclosed {
		return 0, io.ErrClosedPipe
	}
	h.buf = append(h.buf, p...)
	h.cond.Broadcast()
	return len(p), nil
}

func (h *half) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(h.buf) == 0 && !h.wclosed && !h.rclosed {
		h.cond.Wait()
	}
	if h.rclosed {
		return 0, io.ErrClosedPipe
	}
	if len(h.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, h.buf)
	h.buf = h.buf[n:]
	return n, nil
}

func (h *half) closeWrite() {
	h.mu.Lock()
	h.wclosed = true
	h.cond.Broadcast()
	h.mu.Unlock()
}

func (h *half) closeRead() {
	h.mu.Lock()
	h.rclosed = true
	h.buf = nil
	h.cond.Broadcast()
	h.mu.Unlock()
}

// end is one side of the connection.
type end struct {
	r, w *half
}

func (e end) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e end) Write(p []byte) (int, error) { return e.w.Write(p) }

func (e end) Close() error {
	e.w.closeWrite()
	e.r.closeRead()
	return nil
}

func newConn() (client, device io.ReadWriteCloser) {
	c2d, d2c := newHalf(), newHalf()
	return end{r: d2c, w: c2d}, end{r: c2d, w: d2c}
}
