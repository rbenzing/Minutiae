package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
)

// taggedLog records every write of stdout and stderr in one order.
type taggedLog struct {
	mu     sync.Mutex
	events []taggedWrite
}

type taggedWrite struct {
	stream string
	data   string
}

type taggedWriter struct {
	log    *taggedLog
	stream string
}

func (w taggedWriter) Write(p []byte) (int, error) {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	w.log.events = append(w.log.events, taggedWrite{w.stream, string(p)})
	return len(p), nil
}

func (l *taggedLog) joined(stream string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, e := range l.events {
		if e.stream == stream {
			b.WriteString(e.data)
		}
	}
	return b.String()
}

// runTagged runs the CLI with the status display forced to mode, a fast refresh and tagged streams.
func runTagged(ctx context.Context, t *testing.T, mode TerminalMode, args ...string) (int, *taggedLog) {
	t.Helper()
	old := statusInterval
	statusInterval = time.Millisecond
	t.Cleanup(func() { statusInterval = old })
	log := &taggedLog{}
	d := Deps{
		In: strings.NewReader(""), Out: taggedWriter{log, "out"}, Err: taggedWriter{log, "err"},
		Registry: device.NewRegistry(), ErrTerminal: mode,
	}
	root := newRootCmd(d)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil {
		_, _ = d.Err.Write([]byte("error: " + err.Error() + "\n"))
	}
	return ExitCode(err), log
}

func copyCase(t *testing.T, dir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.CopyFS(dst, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestStatusLineClearedBeforeStdout(t *testing.T) {
	dir := cleanVerifyCase(t)
	off := copyCase(t, dir)
	code, log := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", dir)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, log.events)
	}
	offCode, offLog := runTagged(context.Background(), t, TerminalOff, "case", "verify", "--case", off)
	if offCode != 0 {
		t.Fatalf("off: exit %d", offCode)
	}
	if got, want := log.joined("out"), offLog.joined("out"); got != want {
		t.Errorf("stdout differs with the status on:\n%q\n%q", got, want)
	}
	// the first stdout byte comes after the clear sequence, and nothing is drawn after it
	var seenOut, drawn bool
	var lastErr string
	for _, e := range log.events {
		switch e.stream {
		case "err":
			if seenOut && !strings.HasPrefix(e.data, "verify ") {
				t.Errorf("status drawn after stdout began: %q", e.data)
			}
			if !seenOut {
				drawn = true
				lastErr = e.data
			}
		case "out":
			if !seenOut {
				seenOut = true
				if drawn && (!strings.HasPrefix(lastErr, "\r") || !strings.HasSuffix(lastErr, "\r") || strings.TrimSpace(lastErr) != "") {
					t.Errorf("the last write before stdout is %q, want the clear sequence", lastErr)
				}
			}
		}
	}
	if !drawn {
		t.Skip("verify finished before the first refresh; the clear sequence was not exercised")
	}
}

func TestCaseVerifyOutcomeLines(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		code, log := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", cleanVerifyCase(t))
		if errs := log.joined("err"); code != 0 || !strings.HasSuffix(errs, "verify finished: OK\n") {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("one problem", func(t *testing.T) {
		dir := cleanVerifyCase(t)
		if err := os.MkdirAll(filepath.Join(dir, "staging", "ACQ1", "U1"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staging", "ACQ1", "U1", "Manifest.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, log := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", dir)
		if errs := log.joined("err"); code != 4 || !strings.Contains(errs, "verify finished: 1 problem (exit 4)\n") {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		code, log := runTagged(ctx, t, TerminalOn, "case", "verify", "--case", cleanVerifyCase(t))
		if errs := log.joined("err"); code != 1 || !strings.Contains(errs, "verify cancelled (exit 1)\n") {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("integrity", func(t *testing.T) {
		c := newCLICase(t)
		run(t, Deps{}, "case", "info", "--case", c)
		run(t, Deps{}, "case", "info", "--case", c)
		overwriteLine(t, filepath.Join(c, "audit.jsonl"), 2)
		code, log := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", c)
		errs := log.joined("err")
		if code != ExitIntegrity || !strings.Contains(errs, "verify failed: integrity (exit 4)\n") || !strings.Contains(errs, "error:") {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	})
}

func TestCaseVerifyJSONHasNoProgress(t *testing.T) {
	dir := cleanVerifyCase(t)
	off := copyCase(t, dir)
	code, log := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", dir, "--json")
	offCode, offLog := runTagged(context.Background(), t, TerminalOff, "case", "verify", "--case", off, "--json")
	if code != 0 || offCode != 0 {
		t.Fatalf("exit %d and %d", code, offCode)
	}
	if errs := log.joined("err"); errs != "" {
		t.Errorf("--json wrote to stderr: %q", errs)
	}
	if got, want := log.joined("out"), offLog.joined("out"); got != want || got == "" {
		t.Errorf("--json stdout differs:\n%q\n%q", got, want)
	}
}

func TestCaseVerifyOffTerminalHasNoNewOutput(t *testing.T) {
	dir := cleanVerifyCase(t)
	var out, errs bytes.Buffer
	d := Deps{In: strings.NewReader(""), Out: &out, Err: &errs, Registry: device.NewRegistry(), ErrTerminal: TerminalOff}
	if code := Run([]string{"case", "verify", "--case", dir}, d); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if errs.Len() != 0 || strings.Contains(out.String(), "verify finished") {
		t.Errorf("off a terminal: stderr %q, stdout %q", errs.String(), out.String())
	}
	// and the real detection: a buffer is not a terminal
	out.Reset()
	d.ErrTerminal = TerminalAuto
	if code := Run([]string{"case", "verify", "--case", dir}, d); code != 0 || errs.Len() != 0 {
		t.Errorf("auto with a buffer: exit %d, stderr %q", code, errs.String())
	}
}

func TestCaseVerifyOpenErrorHasNoOutcomeLine(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, on := runTagged(context.Background(), t, TerminalOn, "case", "verify", "--case", missing)
	_, off := runTagged(context.Background(), t, TerminalOff, "case", "verify", "--case", missing)
	if got, want := on.joined("err"), off.joined("err"); got != want || !strings.Contains(got, "error:") || strings.Contains(got, "verify ") {
		t.Errorf("open error stderr with the display on %q, off %q", got, want)
	}
}
