package protocol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// fakePort returns queued chunks, then (0, nil) "timeouts", then EOF once eof is set.
type fakePort struct {
	mu            sync.Mutex
	rx            [][]byte
	eof           bool
	eofAfterWrite bool
	readErr       error
	short         bool   // Write accepts one byte less than offered, without an error
	onWrite       func() // called on every Write
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
	if p.onWrite != nil {
		p.onWrite()
	}
	if p.short {
		b = b[:len(b)-1]
	}
	return p.written.Write(b)
}

func (p *fakePort) setEOF() { p.mu.Lock(); p.eof = true; p.mu.Unlock() }

func (p *fakePort) writtenString() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.String()
}

type txResult struct {
	n   int
	err error
}

type memRecorder struct {
	mu      sync.Mutex
	rx, tx  bytes.Buffer
	results []txResult
	cause   error
	txErr   error         // returned by TX when set
	entered chan struct{} // closed when TX is first entered, if set
	release chan struct{} // TX blocks until closed, if set
}

func (m *memRecorder) RX(p []byte) error { m.mu.Lock(); defer m.mu.Unlock(); m.rx.Write(p); return nil }

func (m *memRecorder) TX(p []byte) error {
	if m.entered != nil {
		close(m.entered)
		m.entered = nil
	}
	if m.release != nil {
		<-m.release
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.txErr != nil {
		return m.txErr
	}
	m.tx.Write(p)
	return nil
}

func (m *memRecorder) TXResult(n int, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, txResult{n, err})
	return nil
}

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

func TestRawWriteRecordsOutcome(t *testing.T) {
	port := &fakePort{eofAfterWrite: true}
	rec := &memRecorder{}
	if err := (Raw{}).Run(context.Background(), port, IO{In: strings.NewReader("AT\r"), Out: io.Discard, Rec: rec, AllowWrite: true}); err != nil {
		t.Fatal(err)
	}
	if len(rec.results) != 1 || rec.results[0].n != 3 || rec.results[0].err != nil {
		t.Fatalf("results = %+v", rec.results)
	}
}

func TestRawShortWriteIsRecordedAndReturned(t *testing.T) {
	port := &fakePort{eofAfterWrite: true, short: true}
	rec := &memRecorder{}
	err := Raw{}.Run(context.Background(), port, IO{In: strings.NewReader("AT\r"), Out: io.Discard, Rec: rec, AllowWrite: true})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v", err)
	}
	if len(rec.results) != 1 || rec.results[0].n != 2 || !errors.Is(rec.results[0].err, io.ErrShortWrite) {
		t.Fatalf("results = %+v", rec.results)
	}
}

func TestRawTXAuditFailureBlocksWrite(t *testing.T) {
	errAudit := errors.New("audit log unwritable")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // bounds a regression; the error must end Run first
	defer cancel()
	port := &fakePort{}
	rec := &memRecorder{txErr: errAudit}
	err := Raw{}.Run(ctx, port, IO{In: strings.NewReader("AT\r"), Out: io.Discard, Rec: rec, AllowWrite: true})
	if !errors.Is(err, errAudit) {
		t.Fatalf("err = %v", err)
	}
	if port.writtenString() != "" {
		t.Fatalf("unaudited bytes reached the device: %q", port.writtenString())
	}
}

// A chunk whose TX is in flight when the session ends must either complete
// before Run returns or never be written: no byte may reach the device after
// Run has returned (the recorder is finished right after).
func TestRawNoPortWriteAfterRunReturns(t *testing.T) {
	var runDone, lateWrite atomic.Bool
	wrote := make(chan struct{}, 1)
	port := &fakePort{}
	port.onWrite = func() {
		lateWrite.Store(runDone.Load())
		wrote <- struct{}{}
	}
	rec := &memRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	entered := rec.entered
	done := make(chan error, 1)
	go func() {
		err := Raw{}.Run(context.Background(), port, IO{In: strings.NewReader("x"), Out: io.Discard, Rec: rec, AllowWrite: true})
		runDone.Store(true)
		done <- err
	}()
	<-entered     // TX is in flight
	port.setEOF() // the session ends while it is
	select {
	case <-done: // Run returned with TX still pending: the write would land after it
		runDone.Store(true)
	case <-time.After(100 * time.Millisecond):
	}
	close(rec.release)
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
	}
	if lateWrite.Load() {
		t.Fatal("a byte was written to the port after Run returned")
	}
}

func TestRawWriteRefusedWithoutRecorder(t *testing.T) {
	for name, rec := range map[string]Recorder{"nop": NopRecorder{}, "nil": nil} {
		port := &fakePort{eof: true}
		err := Raw{}.Run(context.Background(), port, IO{In: strings.NewReader("AT\r"), Out: io.Discard, Rec: rec, AllowWrite: true})
		if !errors.Is(err, device.ErrDeviceWriteNotAllowed) {
			t.Errorf("%s: err = %v", name, err)
		}
		if port.writtenString() != "" {
			t.Errorf("%s: wrote %q", name, port.writtenString())
		}
	}
}

func newRecorderCase(t *testing.T) (*evidence.Case, *CaseRecorder) {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	rec, err := NewCaseRecorder(c, "COM7", "acq1")
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func auditEntries(t *testing.T, c *evidence.Case, action string) []evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range es {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestCaseRecorderTXAfterFinishRefused(t *testing.T) {
	c, rec := newRecorderCase(t)
	if err := rec.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if err := rec.TX([]byte("AT\r")); err == nil {
		t.Fatal("TX after Finish succeeded")
	}
	if es := auditEntries(t, c, "device.modify"); len(es) != 0 {
		t.Fatalf("device.modify appended after Finish: %+v", es)
	}
}

func TestCaseRecorderAuditsWriteOutcome(t *testing.T) {
	c, rec := newRecorderCase(t)
	unplugged := errors.New("device disconnected")
	steps := []struct {
		p   string
		n   int
		err error
	}{{"a", 1, nil}, {"bc", 0, unplugged}, {"def", 2, nil}}
	for _, s := range steps {
		if err := rec.TX([]byte(s.p)); err != nil {
			t.Fatal(err)
		}
		if err := rec.TXResult(s.n, s.err); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Finish(nil); err != nil {
		t.Fatal(err)
	}
	mods := auditEntries(t, c, "device.modify")
	if len(mods) != 3 || mods[0].Details["acquisition_id"] != "acq1" {
		t.Fatalf("device.modify = %+v", mods)
	}
	done := auditEntries(t, c, "device.modify.done")
	if len(done) != 1 || fmt.Sprint(done[0].Details["bytes_sent"]) != "1" || done[0].Details["acquisition_id"] != "acq1" {
		t.Fatalf("device.modify.done = %+v", done)
	}
	errs := auditEntries(t, c, "device.modify.error")
	if len(errs) != 2 ||
		fmt.Sprint(errs[0].Details["bytes_sent"]) != "0" || errs[0].Details["error"] != unplugged.Error() ||
		fmt.Sprint(errs[1].Details["bytes_sent"]) != "2" || errs[1].Details["error"] != io.ErrShortWrite.Error() {
		t.Fatalf("device.modify.error = %+v", errs)
	}
}
