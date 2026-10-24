package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestCaseVerifyJSONRefusalKeepsExit4(t *testing.T) {
	old := runVerify
	t.Cleanup(func() { runVerify = old })
	dir := cleanVerifyCase(t)

	runVerify = func(context.Context, Deps, *evidence.Case, evidence.VerifyOptions) (evidence.VerifyReport, error) {
		return evidence.VerifyReport{Problems: []string{"artifact \"a\": bad \xff byte"}}, nil
	}
	code, out, errs := runSplit(context.Background(), t, Deps{}, "--json", "case", "verify", "--case", dir)
	if code != ExitIntegrity {
		t.Errorf("exit %d, want 4\nstdout %q\nstderr %q", code, out, errs)
	}
	if out != "" {
		t.Errorf("stdout %q, want it empty", out)
	}
	for _, want := range []string{`PROBLEM: artifact "a": bad \xff byte`, "FAILED:", errJSONNotUTF8.Error()} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errs)
		}
	}

	runVerify = func(context.Context, Deps, *evidence.Case, evidence.VerifyOptions) (evidence.VerifyReport, error) {
		return evidence.VerifyReport{Notices: []string{"note \xff here"}}, nil
	}
	code, out, errs = runSplit(context.Background(), t, Deps{}, "--json", "case", "verify", "--case", dir)
	if code != ExitError || out != "" || !strings.Contains(errs, `NOTICE: note \xff here`) || !strings.Contains(errs, "OK:") {
		t.Errorf("clean report with a bad notice: exit %d, stdout %q, stderr %q; want exit 1, no stdout, the notice on stderr", code, out, errs)
	}
}

func TestJSONRefusalJoinsAfterTheResultError(t *testing.T) {
	dir := damagedCase(t)
	recordstest.SetRecordColumn(t, dir, 1, "summary", "a\xffb")
	var out, errb bytes.Buffer
	d := Deps{In: strings.NewReader(""), Out: &out, Err: &errb, Registry: device.NewRegistry()}
	root := newRootCmd(d)
	root.SetArgs([]string{"--json", "records", "show", "--case", dir, "1"})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.Is(err, errJSONNotUTF8) {
		t.Fatalf("error %v: want both the integrity failure and the JSON refusal", err)
	}
	if ExitCode(err) != ExitIntegrity || out.Len() != 0 {
		t.Errorf("exit %d, stdout %q; want 4 and nothing written", ExitCode(err), out.String())
	}
	if got := ExitCode(joinJSON(fmt.Errorf("x: %w", evidence.ErrIntegrity), errJSONNotUTF8)); got != ExitIntegrity {
		t.Errorf("integrity joined with a refusal exits %d, want 4", got)
	}
	if got := ExitCode(joinJSON(nil, errJSONNotUTF8)); got != ExitError {
		t.Errorf("a lone refusal exits %d, want 1", got)
	}
	if joinJSON(nil, nil) != nil {
		t.Error("joinJSON(nil, nil) is not nil")
	}
}

// badNameFS lists the root as three files whose third name is not valid UTF-8.
type badNameFS struct{ filesys.FileSystem }

func (b badNameFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	if d.ID != b.FileSystem.Root().ID {
		return b.FileSystem.ReadDir(d)
	}
	var kids []filesys.Entry
	for i, n := range []string{"a", "b", "c\xff"} {
		kids = append(kids, filesys.Entry{Name: n, ID: fmt.Sprintf("nid:%d", 100+i), Type: filesys.TypeFile})
	}
	return kids, nil
}

func (b badNameFS) Underlying() filesys.FileSystem { return b.FileSystem }

func TestStreamingJSONStopsAtTheRefusedElement(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		fsys, err := fstest.Open(r, size)
		if err != nil {
			return nil, err
		}
		return badNameFS{fsys}, nil
	}}}
	code, out, errs := runSplit(context.Background(), t, e.d, "--json", "image", "ls", "-r", "--case", e.c, e.ref)
	if code != ExitError {
		t.Errorf("exit %d, want 1\nstdout %q\nstderr %q", code, out, errs)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out+"]"), &items); err != nil || len(items) != 2 {
		t.Errorf("stdout %q: %d elements (%v), want exactly 2 and nothing after them", out, len(items), err)
	}
	if !strings.Contains(errs, "output truncated after element 2") {
		t.Errorf("stderr lacks the truncation notice:\n%s", errs)
	}

	var nd, ne bytes.Buffer
	s := &jsonStream{w: &nd, errw: &ne}
	s.ndjson(map[string]string{"event": "one"})
	s.ndjson(map[string]string{"event": "bad \xff"})
	s.ndjson(map[string]string{"event": "three"})
	if err := s.finish(); !errors.Is(err, errJSONNotUTF8) {
		t.Errorf("finish() = %v, want errJSONNotUTF8", err)
	}
	if got, want := nd.String(), `{"event":"one"}`+"\n"; got != want {
		t.Errorf("stream wrote %q, want %q", got, want)
	}
	if !strings.Contains(ne.String(), "output truncated after element 1") {
		t.Errorf("stream notice %q", ne.String())
	}
	s2 := &jsonStream{w: io.Discard, errw: &ne}
	s2.ndjson(map[string]string{"event": "fine"})
	if err := s2.finish(); err != nil {
		t.Errorf("clean stream: %v", err)
	}
}

// A failed or refused JSON write is joined after the command's own error: here the write fails (a
// full disk), which takes the same path as a refusal, and the integrity exit survives it.
func TestImageInfoJSONFailureKeepsMismatchExit4(t *testing.T) {
	probe := newEWFEnv(t, ewftest.Options{})
	flipped, _ := hex.DecodeString(probe.md5)
	flipped[0] ^= 1
	e := newEWFEnv(t, ewftest.Options{MD5Override: flipped})
	var errb bytes.Buffer
	d := e.d
	d.Out, d.Err, d.In, d.Registry = failWriter{}, &errb, strings.NewReader(""), device.NewRegistry()
	code := Run([]string{"--json", "image", "info", "--case", e.c, e.recs[0].ID, "--verify"}, d)
	if code != ExitIntegrity || !strings.Contains(errb.String(), "disk full") || !strings.Contains(errb.String(), "stored MD5 differs") {
		t.Errorf("exit %d, stderr %q; want 4 naming both the mismatch and the write failure", code, errb.String())
	}
}

func TestParsePlanJSONFailureKeepsIntegrityExit4(t *testing.T) {
	p := newPfx(t, "a", "b")
	p.flip("a")
	var errb bytes.Buffer
	d := Deps{In: strings.NewReader(""), Out: failWriter{}, Err: &errb, Registry: device.NewRegistry(), ParserRegistry: registry(t, "", good("a"), good("b"))}
	code := Run([]string{"--json", "parse", "plan", "--case", p.dir}, d)
	if code != ExitIntegrity || !strings.Contains(errb.String(), "disk full") || !strings.Contains(errb.String(), "integrity failure") {
		t.Errorf("exit %d, stderr %q; want 4 naming the integrity failure and the write failure", code, errb.String())
	}
}
