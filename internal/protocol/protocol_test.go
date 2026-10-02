package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// fakePort returns queued chunks, then (0, nil) "timeouts", then EOF once eof is set.
type fakePort struct {
	mu            sync.Mutex
	rx            [][]byte
	eof           bool
	eofAfterWrite bool
	readErr       error
	written       bytes.Buffer
}

func (p *fakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rx) > 0 {
		n := copy(b, p.rx[0])
		p.rx = p.rx[1:]
		return n, nil
	}
	if p.readErr != nil {
		return 0, p.readErr
	}
	if p.eof || (p.eofAfterWrite && p.written.Len() > 0) {
		return 0, io.EOF
	}
	p.mu.Unlock()
	time.Sleep(time.Millisecond)
	p.mu.Lock()
	return 0, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.Write(b)
}

type memRecorder struct {
	mu     sync.Mutex
	rx, tx bytes.Buffer
	cause  error
}

func (m *memRecorder) RX(p []byte) error { m.mu.Lock(); defer m.mu.Unlock(); m.rx.Write(p); return nil }

func (m *memRecorder) TX(p []byte) error    { m.mu.Lock(); defer m.mu.Unlock(); m.tx.Write(p); return nil }
func (m *memRecorder) Finish(c error) error { m.cause = c; return nil }

func TestRawReceiveOnly(t *testing.T) {
	port := &fakePort{rx: [][]byte{[]byte("hel"), []byte("lo")}, eof: true}
	var out bytes.Buffer
	rec := &memRecorder{}
	err := Raw{}.Run(context.Background(), port, IO{In: strings.NewReader("typed"), Out: &out, Rec: rec})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" || rec.rx.String() != "hello" {
		t.Fatalf("out=%q rx=%q", out.String(), rec.rx.String())
	}
	if port.written.Len() != 0 || rec.tx.Len() != 0 {
		t.Fatalf("receive-only console wrote %q", port.written.String())
	}
}

func TestRawWriteAllowed(t *testing.T) {
	port := &fakePort{eofAfterWrite: true}
	rec := &memRecorder{}
	err := Raw{}.Run(context.Background(), port, IO{In: strings.NewReader("AT\r"), Out: io.Discard, Rec: rec, AllowWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if port.written.String() != "AT\r" || rec.tx.String() != "AT\r" {
		t.Fatalf("written=%q tx=%q", port.written.String(), rec.tx.String())
	}
}

func TestRawCancelReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := (Raw{}).Run(ctx, &fakePort{}, IO{Out: io.Discard, Rec: NopRecorder{}}); err != nil {
		t.Fatalf("cancel should end cleanly, got %v", err)
	}
}

func TestRawPortErrorPropagates(t *testing.T) {
	unplugged := errors.New("device disconnected")
	err := Raw{}.Run(context.Background(), &fakePort{readErr: unplugged}, IO{Out: io.Discard, Rec: NopRecorder{}})
	if !errors.Is(err, unplugged) {
		t.Fatalf("err = %v", err)
	}
}

func TestCaseRecorderWritesArtifactsAndAudits(t *testing.T) {
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rec, err := NewCaseRecorder(c, "/dev/ttyUSB0", "acq1")
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RX([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := rec.TX([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if err := rec.Finish(nil); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	if len(m) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	rx, _ := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(m[0].Path)))
	tr, _ := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(m[1].Path)))
	if string(rx) != "ab" || strings.Count(string(tr), "\n") != 2 || !strings.Contains(string(tr), `"dir":"tx"`) {
		t.Fatalf("rx=%q transcript=%q", rx, tr)
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if !bytes.Contains(audit, []byte(`"action":"device.modify"`)) {
		t.Fatal("TX was not audited")
	}
	if !strings.Contains(m[0].Path, "_dev_ttyUSB0") {
		t.Fatalf("device id not sanitized: %s", m[0].Path)
	}
}
