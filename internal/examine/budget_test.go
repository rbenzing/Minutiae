package examine

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestBudgetAdmitMath(t *testing.T) {
	type step struct {
		size int64
		want string
	}
	tests := []struct {
		name     string
		files    int
		bytes    int64
		steps    []step
		wantF    int
		wantByte int64
	}{
		{"exactly at the byte limit is admitted", 5, 100, []step{{60, ""}, {40, ""}}, 2, 100},
		{"one byte over is refused", 5, 100, []step{{60, ""}, {41, "max-bytes"}}, 1, 60},
		{"a refusal counts nothing", 5, 100, []step{{101, "max-bytes"}, {100, ""}}, 1, 100},
		{"MaxInt64 size is refused", 5, 100, []step{{math.MaxInt64, "max-bytes"}}, 0, 0},
		{"bytes plus size overflow is refused not wrapped", 5, math.MaxInt64, []step{{math.MaxInt64 - 1, ""}, {math.MaxInt64, "max-bytes"}, {2, "max-bytes"}, {1, ""}}, 2, math.MaxInt64},
		{"exactly at the files limit is admitted", 2, 1000, []step{{1, ""}, {1, ""}}, 2, 2},
		{"one file over is refused", 2, 1000, []step{{1, ""}, {1, ""}, {1, "max-files"}}, 2, 2},
		{"files limit wins over bytes when both are exceeded", 1, 10, []step{{1, ""}, {100, "max-files"}}, 1, 1},
		{"zero-size files count as files", 2, 10, []step{{0, ""}, {0, ""}, {0, "max-files"}}, 2, 0},
		{"a negative size is refused", 5, 100, []step{{-1, "max-bytes"}, {math.MinInt64, "max-bytes"}}, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := newBudget(tc.files, tc.bytes)
			if err != nil {
				t.Fatal(err)
			}
			for i, s := range tc.steps {
				if got := b.admit(s.size); got != s.want {
					t.Fatalf("step %d: admit(%d) = %q, want %q", i, s.size, got, s.want)
				}
			}
			if b.files != tc.wantF || b.bytes != tc.wantByte {
				t.Errorf("counted %d files, %d bytes; want %d, %d", b.files, b.bytes, tc.wantF, tc.wantByte)
			}
		})
	}

	t.Run("zero means the defaults", func(t *testing.T) {
		b, err := newBudget(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if b.maxFiles != DefaultMaxFiles || b.maxBytes != DefaultMaxBytes {
			t.Errorf("defaults = %d files, %d bytes; want %d, %d", b.maxFiles, b.maxBytes, DefaultMaxFiles, DefaultMaxBytes)
		}
		if DefaultMaxFiles != 100_000 || DefaultMaxBytes != 8<<30 {
			t.Errorf("default constants changed: %d files, %d bytes", DefaultMaxFiles, DefaultMaxBytes)
		}
	})
	t.Run("negative is an error", func(t *testing.T) {
		for _, args := range [][2]int64{{-1, 0}, {0, -1}, {-5, -5}, {1, math.MinInt64}} {
			b, err := newBudget(int(args[0]), args[1])
			if b != nil || !errors.Is(err, ErrInvalidLimit) {
				t.Errorf("newBudget(%d, %d) = %v, %v; want nil and ErrInvalidLimit", args[0], args[1], b, err)
			}
		}
	})
	t.Run("a custom limit replaces the default only for its own field", func(t *testing.T) {
		b, err := newBudget(3, 0)
		if err != nil || b.maxFiles != 3 || b.maxBytes != DefaultMaxBytes {
			t.Errorf("newBudget(3, 0) = %+v, %v", b, err)
		}
		b, err = newBudget(0, 7)
		if err != nil || b.maxFiles != DefaultMaxFiles || b.maxBytes != 7 {
			t.Errorf("newBudget(0, 7) = %+v, %v", b, err)
		}
	})
}

// A refusal is final: the caller (the planner) stops at the first refused candidate and does not try a
// smaller one that would fit, because that would reorder the plan around the cap.
func TestBudgetRefusalIsFinal(t *testing.T) {
	b, err := newBudget(10, 100)
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int64{40, 70, 10, 5} // the second does not fit; the third and fourth would
	var admitted []int64
	var limit string
	for _, s := range sizes {
		if limit = b.admit(s); limit != "" {
			break
		}
		admitted = append(admitted, s)
	}
	if limit != "max-bytes" || !slices.Equal(admitted, []int64{40}) {
		t.Fatalf("stopped with %q after admitting %v; want max-bytes after [40]", limit, admitted)
	}
	if b.files != 1 || b.bytes != 40 {
		t.Errorf("after the refusal counted %d files, %d bytes; want 1, 40", b.files, b.bytes)
	}
	// the refusal itself left no trace, so the same call refuses again with the same answer
	if again := b.admit(70); again != "max-bytes" || b.files != 1 || b.bytes != 40 {
		t.Errorf("repeating the refused size = %q, counted %d/%d", again, b.files, b.bytes)
	}
}

func TestAvailBytesArithmetic(t *testing.T) {
	tests := []struct {
		name    string
		blocks  uint64
		unit    int64
		want    int64
		wantErr bool
	}{
		{"plain", 1000, 4096, 4096000, false},
		{"zero blocks", 0, 4096, 0, false},
		{"one byte unit", 12345, 1, 12345, false},
		{"exactly MaxInt64", math.MaxInt64, 1, math.MaxInt64, false},
		{"product overflows: saturates", math.MaxInt64 / 2, 4096, math.MaxInt64, false},
		{"blocks above MaxInt64: saturates", math.MaxUint64, 1, math.MaxInt64, false},
		{"zero unit is an error", 5, 0, 0, true},
		{"negative unit is an error", 5, -4096, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := availBytes(tc.blocks, tc.unit)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("availBytes(%d, %d) = %d, %v; want %d, err=%v", tc.blocks, tc.unit, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestDiskFreeOnTempDir(t *testing.T) {
	free, err := diskFree(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free <= 0 {
		t.Errorf("diskFree(temp dir) = %d, want a positive number", free)
	}
	if _, err := diskFree(filepath.Join(t.TempDir(), "no", "such", "dir")); err == nil {
		t.Error("diskFree of a missing directory succeeded")
	}
}

func testSession(t *testing.T, free func(string) (int64, error)) *Session {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &Session{Case: c, freeBytes: free}
}

func TestCheckSpace(t *testing.T) {
	const planned = 1000
	need := int64(planned) + freeSpaceReserve
	if freeSpaceReserve != 64<<20 {
		t.Fatalf("freeSpaceReserve = %d, want 64 MiB", freeSpaceReserve)
	}
	var asked string
	probe := func(free int64) func(string) (int64, error) {
		return func(dir string) (int64, error) { asked = dir; return free, nil }
	}
	t.Run("enough", func(t *testing.T) {
		s := testSession(t, probe(need+1))
		if err := s.checkSpace(planned); err != nil {
			t.Fatal(err)
		}
		if asked != s.Case.Dir {
			t.Errorf("probed %q, want the case directory %q", asked, s.Case.Dir)
		}
	})
	t.Run("exactly planned plus reserve", func(t *testing.T) {
		if err := testSession(t, probe(need)).checkSpace(planned); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("one byte short", func(t *testing.T) {
		err := testSession(t, probe(need-1)).checkSpace(planned)
		if !errors.Is(err, ErrInsufficientSpace) {
			t.Fatalf("err = %v, want ErrInsufficientSpace", err)
		}
		for _, n := range []int64{need - 1, planned, freeSpaceReserve} {
			if !strings.Contains(err.Error(), fmt.Sprint(n)) {
				t.Errorf("error %q does not name %d", err, n)
			}
		}
	})
	t.Run("zero planned still needs the reserve", func(t *testing.T) {
		if err := testSession(t, probe(freeSpaceReserve-1)).checkSpace(0); !errors.Is(err, ErrInsufficientSpace) {
			t.Errorf("err = %v", err)
		}
		if err := testSession(t, probe(freeSpaceReserve)).checkSpace(0); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a negative plan is treated as zero", func(t *testing.T) {
		if err := testSession(t, probe(freeSpaceReserve)).checkSpace(-1 << 40); err != nil {
			t.Errorf("err = %v", err)
		}
		if err := testSession(t, probe(freeSpaceReserve-1)).checkSpace(-1 << 40); !errors.Is(err, ErrInsufficientSpace) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("planned plus reserve overflow is insufficient, not wrapped", func(t *testing.T) {
		err := testSession(t, probe(math.MaxInt64)).checkSpace(math.MaxInt64)
		if !errors.Is(err, ErrInsufficientSpace) {
			t.Errorf("err = %v, want ErrInsufficientSpace", err)
		}
	})
	t.Run("a probe error is not swallowed", func(t *testing.T) {
		boom := errors.New("statfs exploded")
		err := testSession(t, func(string) (int64, error) { return 1 << 60, boom }).checkSpace(planned)
		if !errors.Is(err, boom) || errors.Is(err, ErrInsufficientSpace) {
			t.Errorf("err = %v, want the probe error as it is", err)
		}
	})
	t.Run("the default probe is the real one", func(t *testing.T) {
		s := testSession(t, nil)
		if err := s.checkSpace(1); err != nil {
			t.Errorf("a byte on the test machine's temp disk: %v", err)
		}
	})
}

// The recovery run asks before its first capture: nothing is written when the space is not there.
func TestCheckSpaceRefusesBeforeAnyWrite(t *testing.T) {
	s := testSession(t, func(string) (int64, error) { return 10 << 20, nil })
	_, err := runAnalysis(s.Case, "dev", "stub", nil, func(*analysis) error {
		if err := s.checkSpace(1 << 30); err != nil {
			return err
		}
		t.Error("checkSpace allowed a run that does not fit")
		return nil
	})
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	es, rerr := evidence.ReadAuditEntries(filepath.Join(s.Case.Dir, "audit.jsonl"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var seq []string
	var errText string
	for _, e := range es {
		if strings.HasPrefix(e.Action, "analysis.") || strings.HasPrefix(e.Action, "artifact.") {
			seq = append(seq, e.Action)
		}
		if e.Action == "analysis.error" {
			errText, _ = e.Details["error"].(string)
		}
	}
	if !slices.Equal(seq, []string{"analysis.start", "analysis.error"}) {
		t.Errorf("audit actions = %v, want analysis.start then analysis.error", seq)
	}
	if !strings.Contains(errText, ErrInsufficientSpace.Error()) {
		t.Errorf("analysis.error carries %q, want the error text", errText)
	}
	if man, merr := s.Case.Manifest(); merr != nil || len(man) != 0 {
		t.Errorf("manifest = %v, %v; want no artifact", man, merr)
	}
}

func lastEntry(t *testing.T, c *evidence.Case, action string) evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Action == action {
			return es[i]
		}
	}
	t.Fatalf("no %s entry", action)
	return evidence.AuditEntry{}
}

func TestWarnWithAddsKeysAndCounts(t *testing.T) {
	c := testSession(t, nil).Case
	sum, err := runAnalysis(c, "dev", "stub", nil, func(a *analysis) error {
		if err := a.warnWith("/p/x", "no maps", map[string]any{"id": "ent-7", "detail": "stale", "analysis_id": "FORGED", "reason": "FORGED", "path": "FORGED"}); err != nil {
			return err
		}
		return a.warnWith("/p/y", "other", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 2 || len(sum.Warnings) != 2 || sum.Warnings[0] != (Warning{Path: "/p/x", Reason: "no maps"}) {
		t.Errorf("summary = %+v, want two counted skips with the first as given", sum)
	}
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ws []evidence.AuditEntry
	for _, e := range es {
		if e.Action == "analysis.warning" {
			ws = append(ws, e)
		}
	}
	if len(ws) != 2 {
		t.Fatalf("%d analysis.warning entries, want 2", len(ws))
	}
	d := ws[0].Details
	if d["id"] != "ent-7" || d["detail"] != "stale" {
		t.Errorf("extra keys missing: %v", d)
	}
	if d["analysis_id"] != sum.AnalysisID || d["reason"] != "no maps" || d["path"] != "/p/x" {
		t.Errorf("base keys were overridden: %v", d)
	}
	if _, has := ws[1].Details["id"]; has {
		t.Errorf("a warning without extras carries id: %v", ws[1].Details)
	}
}

func TestAnalysisEndCarriesExtraDetails(t *testing.T) {
	setAll := func(a *analysis) {
		a.setEnd("recovered", 3)
		a.setEnd("limit_reached", "max-files")
		a.setEnd("files", 999) // base keys win
		a.setEnd("bytes", 999)
		a.setEnd("skipped", 999)
		a.setEnd("analysis_id", "FORGED")
	}
	check := func(t *testing.T, d map[string]any, sum Summary) {
		t.Helper()
		if fmt.Sprint(d["recovered"]) != "3" || d["limit_reached"] != "max-files" {
			t.Errorf("extra keys missing: %v", d)
		}
		if d["analysis_id"] != sum.AnalysisID || fmt.Sprint(d["files"]) != fmt.Sprint(sum.Files) || fmt.Sprint(d["bytes"]) != fmt.Sprint(sum.Bytes) || fmt.Sprint(d["skipped"]) != fmt.Sprint(sum.Skipped) {
			t.Errorf("base keys were overridden: %v (summary %+v)", d, sum)
		}
	}
	t.Run("end", func(t *testing.T) {
		c := testSession(t, nil).Case
		sum, err := runAnalysis(c, "dev", "stub", nil, func(a *analysis) error {
			a.sum.Files, a.sum.Bytes = 2, 30
			if err := a.warn("/x", "skip"); err != nil {
				return err
			}
			setAll(a)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		check(t, lastEntry(t, c, "analysis.end").Details, sum)
	})
	t.Run("error", func(t *testing.T) {
		c := testSession(t, nil).Case
		boom := errors.New("boom")
		sum, err := runAnalysis(c, "dev", "stub", nil, func(a *analysis) error {
			a.sum.Files, a.sum.Bytes = 2, 30
			setAll(a)
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		d := lastEntry(t, c, "analysis.error").Details
		check(t, d, sum)
		if d["error"] != "boom" {
			t.Errorf("error text = %v", d["error"])
		}
	})
	t.Run("an extra key named error cannot hide the error", func(t *testing.T) {
		c := testSession(t, nil).Case
		_, err := runAnalysis(c, "dev", "stub", nil, func(a *analysis) error {
			a.setEnd("error", "all fine")
			return errors.New("real failure")
		})
		if err == nil {
			t.Fatal("no error")
		}
		if got := lastEntry(t, c, "analysis.error").Details["error"]; got != "real failure" {
			t.Errorf("error = %v, want the real failure", got)
		}
	})
}
