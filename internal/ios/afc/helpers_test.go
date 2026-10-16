package afc_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/ios/afc"
	"github.com/rbenzing/minutiae/internal/ios/afc/afctest"
)

// watchdogAfter is how long a wire test may block before the watchdog closes
// its connection; only the watchdog's own test shortens it.
var watchdogAfter = 10 * time.Second

// testRunner is the part of *testing.T the watchdog needs.
type testRunner interface {
	Errorf(format string, args ...any)
	Cleanup(func())
}

// watchdog fails the test with a named message and closes conns when the test
// is still running after watchdogAfter: a wire test that disagrees with its
// peer would otherwise block until the package timeout (W54).
func watchdog(t testRunner, conns ...io.Closer) {
	timer := time.AfterFunc(watchdogAfter, func() {
		t.Errorf("probe blocked: fake and go-ios disagree on the wire")
		for _, c := range conns {
			_ = c.Close()
		}
	})
	t.Cleanup(func() { timer.Stop() })
}

func rawHeader(entire, this, op uint64) []byte {
	b := []byte("CFA6LPAA")
	for _, v := range []uint64{entire, this, 1, op} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	return b
}

func strs(ss ...string) []byte {
	var b []byte
	for _, s := range ss {
		b = append(append(b, s...), 0)
	}
	return b
}

// hostileSrv returns a client whose device answers operation only (0: every
// operation) with reply and answers the rest normally. With patchNum the
// reply's packet number is set to the number of the request it answers, so a
// hostile reply is wrong in the one way a case names and not also stale.
func hostileSrv(t testing.TB, only uint64, reply []byte, closeAfter, patchNum bool) (*afc.Client, *afctest.Server) {
	n := 0
	srv := &afctest.Server{
		Files:           map[string][]byte{"/f": []byte("data")},
		CloseAfterReply: closeAfter,
		Hook: func(op uint64, _ []byte) ([]byte, bool) {
			n++
			if only != 0 && op != only {
				return nil, false
			}
			out := append([]byte(nil), reply...)
			if patchNum && len(out) >= 32 {
				binary.LittleEndian.PutUint64(out[24:], uint64(n))
			}
			return out, true
		},
	}
	rw := afctest.Pipe(srv)
	watchdog(t, rw)
	c := afc.New(rw)
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func hostile(t testing.TB, only uint64, reply []byte, closeAfter bool) *afc.Client {
	c, _ := hostileSrv(t, only, reply, closeAfter, true)
	return c
}

// peer is a scripted device: it records every request it receives.
type peer struct {
	mu   sync.Mutex
	reqs [][]byte
}

func (p *peer) requests() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.reqs...)
}

// newPeer returns a client talking to a goroutine that answers request n
// (1-based) with whatever reply returns; nil sends nothing, closeAfter closes
// the connection after the reply.
func newPeer(t testing.TB, reply func(n int, req []byte) (out []byte, closeAfter bool)) (*afc.Client, *peer) {
	cl, srv := net.Pipe()
	watchdog(t, cl, srv)
	p := &peer{}
	go func() {
		defer func() { _ = srv.Close() }()
		for n := 1; ; n++ {
			var h [40]byte
			if _, err := io.ReadFull(srv, h[:]); err != nil {
				return
			}
			entire := binary.LittleEndian.Uint64(h[8:])
			if entire < 40 || entire > 1<<20 {
				return
			}
			req := make([]byte, entire)
			copy(req, h[:])
			if _, err := io.ReadFull(srv, req[40:]); err != nil {
				return
			}
			p.mu.Lock()
			p.reqs = append(p.reqs, req)
			p.mu.Unlock()
			out, closeAfter := reply(n, req)
			if len(out) > 0 {
				if _, err := srv.Write(out); err != nil {
					return
				}
			}
			if closeAfter {
				return
			}
		}
	}()
	c := afc.New(cl)
	t.Cleanup(func() { _ = c.Close() })
	return c, p
}

// within runs f and reports false if it has not returned after d.
func within(d time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// recorder is a testRunner that records Errorf messages.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recorder) Cleanup(func()) {}
