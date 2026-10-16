package afc_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/ios/afc"
	"github.com/rbenzing/minutiae/internal/ios/afc/afctest"
)

// oneByteConn delivers at most one byte per Read, as a socket may.
type oneByteConn struct{ io.ReadWriteCloser }

func (o oneByteConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.ReadWriteCloser.Read(p[:1])
}

// W57: every read of the client's path (header, header payload, body, file
// bytes across several Reads) must survive a connection that hands over one
// byte at a time.
func TestClientReadsThroughAOneBytePerReadConnection(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 300) // several reads of a small buffer
	srv := &afctest.Server{Files: map[string][]byte{"/d/a.bin": data, "/d/b": []byte("x")}}
	rw := afctest.Pipe(srv)
	watchdog(t, rw)
	c := afc.New(oneByteConn{rw})
	t.Cleanup(func() { _ = c.Close() })
	var names []string
	var st afc.Stat
	var got []byte
	if !within(callBound, func() {
		var err error
		if names, err = c.List("/d"); err != nil {
			t.Errorf("List: %v", err)
			return
		}
		if st, err = c.Stat("/d/a.bin"); err != nil {
			t.Errorf("Stat: %v", err)
			return
		}
		f, err := c.Open("/d/a.bin")
		if err != nil {
			t.Errorf("Open: %v", err)
			return
		}
		buf := make([]byte, 7)
		for {
			n, err := f.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				if err != io.EOF {
					t.Errorf("Read: %v", err)
				}
				break
			}
		}
		if err := f.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}) {
		t.Fatal("the client blocked on a one-byte-per-Read connection")
	}
	if len(names) != 2 || names[0] != "a.bin" || names[1] != "b" {
		t.Errorf("names = %q", names)
	}
	if st.Size != int64(len(data)) || !st.IsRegular() {
		t.Errorf("Stat = %+v", st)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("read %d bytes, want %d identical", len(got), len(data))
	}
}

// W58: the watchdog works over afctest.Pipe, the connection the wire tests
// use, when the device and the client disagree: a silent device blocks the
// call, the watchdog names the probe and closes the pipe, and the call fails
// instead of hanging.
func TestWatchdogUnblocksAClientOverAfctestPipe(t *testing.T) {
	old := watchdogAfter
	watchdogAfter = 50 * time.Millisecond
	defer func() { watchdogAfter = old }()
	srv := &afctest.Server{Hook: func(uint64, []byte) ([]byte, bool) { return nil, true }} // never answers
	rw := afctest.Pipe(srv)
	rec := &recorder{}
	watchdog(rec, rw)
	c := afc.New(rw)
	var err error
	if !within(callBound, func() { _, err = c.List("/") }) {
		t.Fatal("the watchdog did not unblock a call over afctest.Pipe")
	}
	if err == nil {
		t.Fatal("List against a silent device returned no error")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "probe blocked: fake and go-ios disagree on the wire") {
		t.Fatalf("watchdog messages = %q", rec.msgs)
	}
}
