package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/transport/serial"
)

type cliFakePort struct {
	mu   sync.Mutex
	data []byte
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
func (p *cliFakePort) Close() error                { return nil }
func (p *cliFakePort) SetDTR(bool) error           { return nil }
func (p *cliFakePort) SetRTS(bool) error           { return nil }

type cliFakeProvider struct{ opened string }

func (f *cliFakeProvider) List() ([]serial.PortInfo, error) {
	return []serial.PortInfo{{Name: "/dev/ttyUSB0", IsUSB: true, VID: "0403", PID: "6001", Product: "FT232R"}}, nil
}

func (f *cliFakeProvider) Open(name string, _ serial.Config) (serial.Port, error) {
	f.opened = name
	return &cliFakePort{data: []byte("U-Boot 2024.01\n")}, nil
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
