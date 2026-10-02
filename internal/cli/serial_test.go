package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/transport/serial"
)

type cliFakePort struct {
	mu      sync.Mutex
	data    []byte
	onClose func()
}

func (p *cliFakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.data)
	p.data = p.data[n:]
	return n, nil
}
func (p *cliFakePort) Write(b []byte) (int, error) { return len(b), nil }
func (p *cliFakePort) SetDTR(bool) error           { return nil }
func (p *cliFakePort) SetRTS(bool) error           { return nil }
func (p *cliFakePort) Close() error {
	if p.onClose != nil {
		p.onClose()
	}
	return nil
}

type cliFakeProvider struct {
	opened  string
	cfg     serial.Config
	openErr error
	onOpen  func()
	onClose func()
}

func (f *cliFakeProvider) List() ([]serial.PortInfo, error) {
	return []serial.PortInfo{{Name: "/dev/ttyUSB0", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A10K", Product: "FT232R"}}, nil
}

func (f *cliFakeProvider) Open(name string, cfg serial.Config) (serial.Port, error) {
	if f.onOpen != nil {
		f.onOpen()
	}
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened, f.cfg = name, cfg
	return &cliFakePort{data: []byte("U-Boot 2024.01\n"), onClose: f.onClose}, nil
}

func auditOf(t *testing.T, caseDir string) []evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(caseDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func findAction(es []evidence.AuditEntry, action string) (evidence.AuditEntry, bool) {
	for _, e := range es {
		if e.Action == action {
			return e, true
		}
	}
	return evidence.AuditEntry{}, false
}

func TestSerialList(t *testing.T) {
	code, out := run(t, Deps{Serial: &cliFakeProvider{}}, "serial", "list")
	if code != 0 || !strings.Contains(out, "/dev/ttyUSB0") || !strings.Contains(out, "0403:6001") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestSerialConsoleRecordsSession(t *testing.T) {
	c := newCLICase(t)
	p := &cliFakeProvider{}
	code, out := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c)
	if code != 0 || !strings.Contains(out, "U-Boot 2024.01") || p.opened != "/dev/ttyUSB0" {
		t.Fatalf("%d %s", code, out)
	}
	manifest, _ := os.ReadFile(filepath.Join(c, "manifest.jsonl"))
	if strings.Count(string(manifest), "\n") != 2 || !strings.Contains(string(manifest), "rx.bin") {
		t.Fatalf("manifest = %s", manifest)
	}
	audit, _ := os.ReadFile(filepath.Join(c, "audit.jsonl"))
	if !strings.Contains(string(audit), `"type":"serial.session"`) {
		t.Fatalf("session not audited: %s", audit)
	}
}

func TestSerialConsoleWriteNeedsCase(t *testing.T) {
	code, _ := run(t, Deps{Serial: &cliFakeProvider{}}, "serial", "console", "--port", "COM3", "--allow-device-write")
	if code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}

func TestSerialConsoleBadConfigIsUsage(t *testing.T) {
	p := &cliFakeProvider{}
	code, _ := run(t, Deps{Serial: p}, "serial", "console", "--port", "COM3", "--parity", "foo")
	if code != ExitUsage || p.opened != "" {
		t.Fatalf("code = %d opened = %q", code, p.opened)
	}
}

func TestSerialConsoleModemLinesOffByDefault(t *testing.T) {
	c := newCLICase(t)
	p := &cliFakeProvider{}
	if code, out := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if p.cfg.DTR || p.cfg.RTS {
		t.Fatalf("provider asked to assert modem lines: DTR=%v RTS=%v", p.cfg.DTR, p.cfg.RTS)
	}
	es := auditOf(t, c)
	start, _ := findAction(es, "acquire.start")
	d := start.Details
	if d["dtr"] != false || d["rts"] != false || fmt.Sprint(d["read_timeout_ms"]) != "100" || d["host_os"] != runtime.GOOS {
		t.Fatalf("acquire.start details = %v", d)
	}
	if _, ok := findAction(es, "device.modify"); ok {
		t.Fatal("receive-only session with default lines recorded a device.modify")
	}
}

func TestSerialConsoleModemLinesNeedAllowWrite(t *testing.T) {
	c := newCLICase(t)
	for _, flag := range []string{"--dtr", "--rts"} {
		p := &cliFakeProvider{}
		code, _ := run(t, Deps{Serial: p}, "serial", "console", "--port", "COM3", "--case", c, flag)
		if code != ExitUsage || p.opened != "" {
			t.Fatalf("%s: code = %d opened = %q", flag, code, p.opened)
		}
	}
}

func TestSerialConsoleModemLinesAuditedBeforeOpen(t *testing.T) {
	c := newCLICase(t)
	p := &cliFakeProvider{}
	var auditedAtOpen bool
	p.onOpen = func() {
		e, ok := findAction(auditOf(t, c), "device.modify")
		auditedAtOpen = ok && e.Details["operation"] == "serial.line_state" && e.Details["dtr"] == true && e.Details["rts"] == false
	}
	code, out := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c, "--allow-device-write", "--dtr")
	if code != 0 || !p.cfg.DTR || p.cfg.RTS {
		t.Fatalf("%d %s cfg=%+v", code, out, p.cfg)
	}
	if !auditedAtOpen {
		t.Fatal("asserting DTR was not audited as device.modify serial.line_state before open")
	}
}

func TestSerialConsoleBadCaseNeverOpensPort(t *testing.T) {
	p := &cliFakeProvider{}
	code, _ := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", filepath.Join(t.TempDir(), "nope"))
	if code == ExitOK || p.opened != "" {
		t.Fatalf("code = %d opened = %q", code, p.opened)
	}
}

func TestSerialConsoleOpenErrorIsAudited(t *testing.T) {
	c := newCLICase(t)
	p := &cliFakeProvider{openErr: errors.New("access denied")}
	code, out := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c)
	if code == ExitOK || strings.Contains(out, "connected") {
		t.Fatalf("%d %s", code, out)
	}
	e, ok := findAction(auditOf(t, c), "acquire.error")
	if !ok || !strings.Contains(fmt.Sprint(e.Details["error"]), "access denied") {
		t.Fatalf("open failure not audited: %+v", e)
	}
}

func TestSerialConsoleClosesPortBeforeAcquireEnd(t *testing.T) {
	c := newCLICase(t)
	p := &cliFakeProvider{}
	endedBeforeClose := true
	p.onClose = func() { _, endedBeforeClose = findAction(auditOf(t, c), "acquire.end") }
	if code, out := run(t, Deps{Serial: p}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if endedBeforeClose {
		t.Fatal("port was still open when acquire.end was recorded")
	}
}

func TestSerialConsoleRecordsUSBIdentity(t *testing.T) {
	c := newCLICase(t)
	if code, out := run(t, Deps{Serial: &cliFakeProvider{}}, "serial", "console", "--port", "/dev/ttyUSB0", "--case", c); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	start, _ := findAction(auditOf(t, c), "acquire.start")
	d := start.Details
	if d["vid"] != "0403" || d["pid"] != "6001" || d["usb_serial"] != "A10K" || d["product"] != "FT232R" {
		t.Fatalf("acquire.start details = %v", d)
	}
}
