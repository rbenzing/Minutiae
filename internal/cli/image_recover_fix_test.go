package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// lineOf returns the first line of out that contains sub.
func lineOf(out, sub string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}

func markerNodes() []fstest.Node {
	return []fstest.Node{
		recNode("/part.bin", recPat(1, 4*recBS), fstest.RecoverMap{Method: recMethod, Size: 6 * recBS}), // claims more than its blocks
		recNode("/ok.bin", recPat(2, recBS)),
		recNode("/enc.bin", recPat(3, 2*recBS), fstest.RecoverMap{Method: recMethod, Encrypted: true}),
	}
}

// C61 I1: the run-mode markers, the totals and the confidence are per line, not "somewhere in the output".
func TestImageRecoverRunMarkersAndTotals(t *testing.T) {
	e := newRecoverEnv(t, markerNodes()...)
	code, out := e.recover(t, "--all")
	if code != 0 {
		t.Fatalf("run: %d %s", code, out)
	}
	part, ok, enc := lineOf(out, "/part.bin"), lineOf(out, "/ok.bin"), lineOf(out, "/enc.bin")
	if !strings.Contains(part, "[incomplete: ") || strings.Contains(part, "[encrypted]") {
		t.Errorf("partial line %q", part)
	}
	if strings.Contains(ok, "[incomplete") || strings.Contains(ok, "[encrypted]") || strings.Contains(ok, "[overlap]") {
		t.Errorf("plain line %q carries a marker", ok)
	}
	if !strings.Contains(enc, "[encrypted]") || strings.Contains(enc, "[incomplete") {
		t.Errorf("encrypted line %q", enc)
	}
	wantBytes := int64(4*recBS + recBS + 2*recBS)
	if want := fmt.Sprintf("recovered 3 (%d bytes), partial 1", wantBytes); !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	for _, r := range recoveredRecords(t, e.c) {
		conf := *r.Source.Derived.Recovery.Confidence
		if conf == 0 {
			t.Fatalf("%s has confidence 0: the test cannot tell a printed 0 from the real value", r.Path)
		}
		line := lineOf(out, r.Source.Derived.FSPath)
		if !strings.Contains(line, fmt.Sprintf("conf %d ", conf)) {
			t.Errorf("%s: line %q lacks conf %d", r.Source.Derived.FSPath, line, conf)
		}
	}
	if strings.Contains(out, "limit reached") || strings.Contains(out, "filesystem warnings") {
		t.Errorf("a clean run prints a limit or filesystem-warning line:\n%s", out)
	}

	// the same markers in list mode, and the totals there
	code, out = e.recover(t, "--list")
	if code != 0 {
		t.Fatal(out)
	}
	if l := lineOf(out, "/ok.bin"); strings.Contains(l, "[encrypted]") || strings.Contains(l, "[incomplete") {
		t.Errorf("list plain line %q carries a marker", l)
	}
	if l := lineOf(out, "/enc.bin"); !strings.Contains(l, "[encrypted]") {
		t.Errorf("list encrypted line %q", l)
	}
	if strings.Contains(out, "limit reached") || strings.Contains(out, "filesystem warnings") {
		t.Errorf("a clean list prints a limit or filesystem-warning line:\n%s", out)
	}
}

func TestImageRecoverLimitLineAppearsOnlyWithALimit(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	for _, mode := range []string{"--list", "--all"} {
		code, out := e.recover(t, mode, "--max-files", "1")
		if code != 0 || !strings.Contains(out, "limit reached: max-files (1 not processed)") {
			t.Errorf("%s --max-files 1: exit %d, want a limit line with the count:\n%s", mode, code, out)
		}
	}
}

// C61 M6: JSON carries the not-processed count and the filesystem-warning count, as the text mode does.
func TestImageRecoverJSONHasNotProcessedAndFSWarnings(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	for _, mode := range []string{"--list", "--all"} {
		code, out := e.recover(t, mode, "--json", "--max-files", "1")
		if code != 0 {
			t.Fatalf("%s: %d %s", mode, code, out)
		}
		var m map[string]any
		if err := json.Unmarshal(jsonPart(out), &m); err != nil {
			t.Fatalf("%s json %q: %v", mode, out, err)
		}
		if m["limit_reached"] != "max-files" || m["not_processed"] != float64(1) {
			t.Errorf("%s: limit_reached %v not_processed %v, want max-files and 1", mode, m["limit_reached"], m["not_processed"])
		}
		if v, present := m["fs_warnings"]; !present || v != float64(0) {
			t.Errorf("%s: fs_warnings = %v (present %v), want 0", mode, v, present)
		}
	}
}

// walkFailFS fails to list the directory named "sub..." with an error whose text is hostile.
type walkFailFS struct{ filesys.FileSystem }

func (w walkFailFS) Underlying() filesys.FileSystem { return w.FileSystem }

const hostileText = "bad\x1b[2J\u202eevil\u0085\nline"

func (w walkFailFS) ReadDir(e filesys.Entry) ([]filesys.Entry, error) {
	if strings.HasPrefix(e.Name, "sub") {
		return nil, fmt.Errorf("cannot list %s: %w", hostileText, filesys.ErrCorrupt)
	}
	return w.FileSystem.ReadDir(e)
}

var walkFailDrivers = []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fsys, err := fstest.Open(r, size)
	if err != nil {
		return nil, err
	}
	return walkFailFS{fsys}, nil
}}}

// C61 I2: the skip detail, the skip path and the method from the image never reach the terminal raw.
func TestImageRecoverEscapesSkipDetailPathAndMethod(t *testing.T) {
	nodes := []fstest.Node{
		recNode("/ok.bin", recPat(1, recBS)),
		{Path: "/sub\x1b[31mdir\u202e", Dir: true},
		recNode("/meth.bin", recPat(2, recBS), fstest.RecoverMap{Method: "m\x1b[2Jx\u202e"}),
	}
	e := newRecoverEnv(t, nodes...)
	e.d = Deps{FSDrivers: walkFailDrivers}
	sawSkip := false
	for _, args := range [][]string{{"--list"}, {"--all"}} {
		var stdout, stderr bytes.Buffer
		d := Deps{FSDrivers: walkFailDrivers, Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
		code := Run(append([]string{"image", "recover", "--case", e.c, e.ref}, args...), d)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, stdout.String(), stderr.String())
		}
		for name, s := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
			if hasRawControl(s) {
				t.Errorf("%v %s holds a raw control or format rune: %q", args, name, s)
			}
		}
		if args[0] == "--list" {
			sawSkip = strings.Contains(stdout.String(), "[skipped: ") && strings.Contains(stdout.String(), "cannot list")
		}
	}
	if !sawSkip {
		t.Error("the list shows no walk-error skip with its detail: the hostile detail was never exercised")
	}
}

// C61 I2: the artifact error and the limit name are escaped too (unit level: a run summary built by hand).
func TestPrintRecoverRunEscapesErrorAndLimit(t *testing.T) {
	conf := 20
	var sum examine.RecoverSummary
	sum.Artifacts = []evidence.ManifestRecord{{
		Size: 10, Incomplete: true, Error: "stopped \x1b[2J\u202e at \nhere",
		Source: evidence.Source{Kind: evidence.KindRecover, Derived: &evidence.Derivation{
			FSPath: "/a", FSID: "1", Recovery: &evidence.Recovery{Method: "fat-contiguous", Confidence: &conf},
		}},
	}}
	sum.LimitReached = "max\x1b[2J"
	var out bytes.Buffer
	printRecoverRun(&out, sum)
	if hasRawControl(out.String()) {
		t.Errorf("raw control in %q", out.String())
	}
	if !strings.Contains(out.String(), "[incomplete: stopped ") || !strings.Contains(out.String(), "limit reached: max") {
		t.Errorf("the markers are missing from %q", out.String())
	}
}

// C61 M2: the boundaries of --min-confidence and an empty reference.
func TestImageRecoverBoundaryArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"confidence 100", []string{"--all", "--min-confidence", "100"}, 0},
		{"confidence 0", []string{"--all", "--min-confidence", "0"}, 0},
		{"empty ref", []string{""}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRecoverEnv(t, twoDeleted()...)
			if code, out := e.recover(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d:\n%s", code, tc.want, out)
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// C61 M3: a JSON write failure is reported, but never replaces the run's own error (an integrity error).
func TestWriteRecoverJSONNeverMasksTheRunError(t *testing.T) {
	var sum examine.RecoverSummary
	integrity := fmt.Errorf("x: %w", evidence.ErrIntegrity)
	if err := writeRecoverJSON(failWriter{}, sum, nil); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("a write failure of a clean run = %v, want it reported", err)
	}
	if err := writeRecoverJSON(failWriter{}, sum, integrity); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("a write failure replaced the integrity error: %v", err)
	}
	var ok bytes.Buffer
	if err := writeRecoverJSON(&ok, sum, integrity); !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(ok.String(), "analysis_id") {
		t.Errorf("good writer: %v %q", err, ok.String())
	}
	if err := writeRecoverJSON(&ok, sum, nil); err != nil {
		t.Errorf("clean: %v", err)
	}
}

// C61 M4: only an unsupported-recovery error is wrapped with the filesystem name.
func TestWithFSNameLeavesOtherErrorsAlone(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("x: %w", evidence.ErrIntegrity),
		errors.New("plain"),
		fmt.Errorf("x: %w", filesys.ErrNotDeleted),
	} {
		if got := withFSName(nil, 0, err); got != err { //nolint:errorlint // identity is the point
			t.Errorf("withFSName changed %v into %v", err, got)
		}
	}
	if withFSName(nil, 0, nil) != nil {
		t.Error("nil became non-nil")
	}
}

// C61 M5: the help says what --list writes, and names the defaults of the caps.
func TestImageRecoverHelpIsAccurate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := Deps{FSDrivers: mtfsDrivers, Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
	if code := Run([]string{"image", "recover", "--help"}, d); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	h := stdout.String() + stderr.String()
	for _, want := range []string{
		"writes nothing but the usual case.open audit entry",
		fmt.Sprintf("default %d", examine.DefaultMaxFiles),
		fmt.Sprintf("default %d", examine.DefaultMaxBytes),
	} {
		if !strings.Contains(h, want) {
			t.Errorf("help lacks %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "changes nothing in the case") {
		t.Error("help still says --list changes nothing in the case (it appends case.open)")
	}
}
