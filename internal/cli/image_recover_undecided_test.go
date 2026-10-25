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
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// answerFS makes Recoverable return answers[name] for the deleted entry of that name.
type answerFS struct {
	filesys.FileSystem
	answers map[string]error
}

func (f answerFS) Underlying() filesys.FileSystem { return f.FileSystem }

func (f answerFS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	if err, ok := f.answers[e.Name]; ok {
		return nil, err
	}
	inner, ok := filesys.As[filesys.Recoverer](f.FileSystem)
	if !ok {
		return nil, filesys.ErrNoRecovery
	}
	return inner.Recoverable(e)
}

func answerDrivers(answers map[string]error) []detect.Driver {
	return []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		fsys, err := fstest.Open(r, size)
		if err != nil {
			return nil, err
		}
		return answerFS{FileSystem: fsys, answers: answers}, nil
	}}}
}

// recoverSplit runs `image recover` with stdout and stderr apart.
func (e *imgEnv) recoverSplit(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	d := e.d
	d.Out, d.Err, d.In, d.Registry = &out, &errb, strings.NewReader(""), device.NewRegistry()
	code = Run(append([]string{"image", "recover", "--case", e.c, e.ref}, args...), d)
	return code, out.String(), errb.String()
}

func answerEnv(t *testing.T, answers map[string]error) *imgEnv {
	t.Helper()
	e := newRecoverEnv(t, twoDeleted()...)
	e.d = Deps{FSDrivers: answerDrivers(answers)}
	return e
}

func TestImageRecoverUndecidedExitsOne(t *testing.T) {
	e := answerEnv(t, map[string]error{"a-del.bin": fmt.Errorf("%w: a name of an unmeasured shape", filesys.ErrUndecided)})
	code, out, errOut := e.recoverSplit("--all")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "undecided 1") || !strings.Contains(out, "recovered 1 ") {
		t.Errorf("summary lacks 'undecided 1' or the other artifact:\n%s", out)
	}
	if !strings.Contains(errOut, "1 deleted entries could not be decided") {
		t.Errorf("error %q does not name the count", errOut)
	}
	if strings.Contains(errOut, "Nothing was skipped") {
		t.Errorf("the lookup note (nothing was skipped) is wrong for a batch:\n%s", errOut)
	}
	if got := len(recoveredRecords(t, e.c)); got != 1 {
		t.Errorf("%d artifacts written, want the decided one", got)
	}
	if vc, vout := run(t, e.d, "case", "verify", "--case", e.c); vc != 0 {
		t.Errorf("case verify exit %d:\n%s", vc, vout)
	}

	// --json: the same run again would collide on ids, so use a fresh case.
	e = answerEnv(t, map[string]error{"a-del.bin": filesys.ErrUndecided})
	code, out, errOut = e.recoverSplit("--all", "--json")
	var rep struct {
		SkippedBy map[string]int `json:"skipped_by"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if code != 1 || rep.SkippedBy["undecided"] != 1 || !strings.Contains(errOut, "1 deleted entries could not be decided") {
		t.Errorf("--json: exit %d, skipped_by %v, stderr %q; want 1, undecided 1 and the count", code, rep.SkippedBy, errOut)
	}
}

func TestImageRecoverListUndecidedExitsOne(t *testing.T) {
	e := answerEnv(t, map[string]error{"b-del.bin": fmt.Errorf("%w: %w", filesys.ErrLimit, filesys.ErrIncomplete)})
	code, out, errOut := e.recoverSplit("--list")
	if code != 1 || !strings.Contains(errOut, "1 deleted entries could not be decided") || !strings.Contains(out, "undecided 1") {
		t.Errorf("--list: exit %d, stdout %q, stderr %q; want 1, 'undecided 1' and the count", code, out, errOut)
	}
	code, out, _ = e.recoverSplit("--list", "--json")
	var rep struct {
		SkippedBy map[string]int `json:"skipped_by"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if code != 1 || rep.SkippedBy["undecided"] != 1 {
		t.Errorf("--list --json: exit %d, skipped_by %v; want 1 and undecided 1", code, rep.SkippedBy)
	}
}

func TestImageRecoverReusedOnlyExitsZero(t *testing.T) {
	e := answerEnv(t, map[string]error{"a-del.bin": fmt.Errorf("%w: inode 12 is allocated", filesys.ErrReused)})
	for _, args := range [][]string{{"--list"}, {"--all"}} {
		code, out, errOut := e.recoverSplit(args...)
		if code != 0 || !strings.Contains(out, "reused 1") || strings.Contains(out, "undecided") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 0 and 'reused 1'", args, code, out, errOut)
		}
	}
}

// The exit code of a run that failed its integrity AND has undecided entries is 4 (ExitCode and the error join only; no seam fails an evidence write mid-run without a new production hook, so the end-to-end case is not tested).
func TestImageRecoverExitCodeBeatsUndecided(t *testing.T) {
	integrity := fmt.Errorf("x: %w", evidence.ErrIntegrity)
	err := undecidedBatchError(integrity, 3)
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.Is(err, filesys.ErrUndecided) {
		t.Fatalf("undecidedBatchError = %v, want both the integrity and the undecided cause", err)
	}
	if got := ExitCode(err); got != 4 {
		t.Errorf("ExitCode = %d, want 4", got)
	}
	if got := ExitCode(undecidedBatchError(nil, 2)); got != 1 {
		t.Errorf("ExitCode of an undecided batch = %d, want 1", got)
	}
	if err := undecidedBatchError(nil, 0); err != nil {
		t.Errorf("no undecided entry gave %v, want nil", err)
	}
	if err := undecidedBatchError(integrity, 0); err != integrity {
		t.Errorf("the run's own error was replaced: %v", err)
	}
}

func TestImageRecoverReusedAndUndecidedMixed(t *testing.T) {
	nodes := append(twoDeleted(), recNode("/c-del.bin", recPat(3, recBS)), recNode("/d-del.bin", recPat(4, recBS)))
	e := newRecoverEnv(t, nodes...)
	e.d = Deps{FSDrivers: answerDrivers(map[string]error{
		"a-del.bin": fmt.Errorf("%w: inode 12 is allocated", filesys.ErrReused),
		"c-del.bin": filesys.ErrUndecided,
		"d-del.bin": fmt.Errorf("%w: %w", filesys.ErrLimit, filesys.ErrIncomplete),
	})}
	code, out, errOut := e.recoverSplit("--all", "--json")
	var rep struct {
		Recovered int            `json:"recovered"`
		SkippedBy map[string]int `json:"skipped_by"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if code != 1 || rep.Recovered != 1 || rep.SkippedBy["reused"] != 1 || rep.SkippedBy["undecided"] != 2 || len(rep.SkippedBy) != 2 {
		t.Errorf("exit %d, recovered %d, skipped_by %v; want 1, 1, reused 1 and undecided 2", code, rep.Recovered, rep.SkippedBy)
	}
	if !strings.Contains(errOut, "2 deleted entries could not be decided") {
		t.Errorf("stderr %q does not name the undecided count (reused must not be counted)", errOut)
	}
}
