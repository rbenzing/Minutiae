package examine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// failByName makes Recoverable return errs[name] for the deleted entry of that name and defer to the reader for
// every other entry.
func failByName(errs map[string]error) func(*fsHook) {
	return func(h *fsHook) {
		h.recoverable = func(in filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			if err, ok := errs[en.Name]; ok {
				return nil, err
			}
			return in.Recoverable(en)
		}
	}
}

func TestRecoverReusedIsSkippedReused(t *testing.T) {
	good := pat(2, 2*bs)
	e, _ := newHookEnv(t, failByName(map[string]error{
		"first.bin": fmt.Errorf("%w: inode 12 is allocated", filesys.ErrReused),
	}), delNode("/first.bin", pat(1, bs)), delNode("/second.bin", good))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["reused"] != 1 || sum.Recovered != 1 || len(sum.SkippedBy) != 1 {
		t.Fatalf("SkippedBy %v, Recovered %d; want exactly one reused skip and one recovery", sum.SkippedBy, sum.Recovered)
	}
	recs := recovered(t, e.c)
	if len(recs) != 1 || !strings.HasSuffix(recs[0].Source.Derived.FSPath, "second.bin") {
		t.Fatalf("recovered %+v, want only second.bin", recs)
	}
	var found bool
	for _, w := range auditByAction(t, e.c, "analysis.warning") {
		if w.Details["code"] == "reused" {
			found = true
			d, _ := w.Details["reason"].(string)
			if !strings.Contains(d, "inode 12 is allocated") || w.Details["id"] == nil {
				t.Errorf("reused warning %v does not name the id and the text", w.Details)
			}
		}
	}
	if !found {
		t.Errorf("no audited reused skip; warnings %v", warnReasons(t, e.c))
	}
}

func TestRecoverUndecidedIsSkippedNotAborted(t *testing.T) {
	e, _ := newHookEnv(t, failByName(map[string]error{
		"u1.bin": fmt.Errorf("%w: a name of an unmeasured shape", filesys.ErrUndecided),
		"u2.bin": fmt.Errorf("%w: %w", filesys.ErrLimit, filesys.ErrIncomplete),
		"u3.bin": filesys.ErrLimit,
		"u4.bin": fmt.Errorf("%w: %w", &filesys.CorruptError{Structure: "x", Reason: "y"}, filesys.ErrIncomplete),
	}), delNode("/u1.bin", pat(1, bs)), delNode("/u2.bin", pat(2, bs)), delNode("/u3.bin", pat(3, bs)),
		delNode("/u4.bin", pat(4, bs)), delNode("/ok.bin", pat(5, bs)))
	sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if sum.SkippedBy["undecided"] != 4 || sum.SkippedBy["corrupt"] != 0 || sum.SkippedBy["not-found"] != 0 || sum.Recovered != 1 {
		t.Fatalf("SkippedBy %v, Recovered %d; want 4 undecided, no corrupt or not-found, 1 recovered", sum.SkippedBy, sum.Recovered)
	}
}

func TestRecoverUndecidedDetailNamesTheCause(t *testing.T) {
	capErr := fmt.Errorf("%w: the search for the directory at cluster 5 stopped after 2 directories", filesys.ErrLimit)
	e, _ := newHookEnv(t, failByName(map[string]error{"a.bin": capErr}), delNode("/a.bin", pat(1, bs)))
	e.recover(t, examine.RecoverOptions{All: true})
	for _, w := range auditByAction(t, e.c, "analysis.warning") {
		if w.Details["code"] == "undecided" {
			if d, _ := w.Details["reason"].(string); !strings.Contains(d, "stopped after 2 directories") {
				t.Errorf("detail %q does not carry the cap text", d)
			}
			return
		}
	}
	t.Errorf("no audited undecided skip; warnings %v", warnReasons(t, e.c))
}

func TestRecoverIOErrorStillEndsTheRun(t *testing.T) {
	ioErr := errors.New("read failed: device gone")
	e, _ := newHookEnv(t, failByName(map[string]error{"a.bin": ioErr}), delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)))
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if !errors.Is(err, ioErr) {
		t.Fatalf("Recover = %v, want the I/O error", err)
	}
	if n := len(auditByAction(t, e.c, "analysis.error")); n != 1 {
		t.Errorf("%d analysis.error entries, want 1", n)
	}
}

func TestRecoverAnalysisEndCountsUndecided(t *testing.T) {
	e, _ := newHookEnv(t, failByName(map[string]error{
		"a.bin": filesys.ErrUndecided,
		"b.bin": fmt.Errorf("%w: inode in use", filesys.ErrReused),
	}), delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)), delNode("/c.bin", pat(3, bs)))
	e.recover(t, examine.RecoverOptions{All: true})
	ends := auditByAction(t, e.c, "analysis.end")
	if len(ends) != 1 {
		t.Fatalf("%d analysis.end entries", len(ends))
	}
	if got := fmt.Sprint(ends[0].Details["skipped_by"]); got != "map[reused:1 undecided:1]" {
		t.Errorf("analysis.end skipped_by = %v, want reused 1 and undecided 1", got)
	}
}

// TestReaderLimitErrorShapesAreUndecided (LX-7b regression): the exact errors the FAT and exFAT readers return at
// their caps used to end the whole run; each is now a counted undecided skip. The texts are those of
// fat/dir.go (locate, cutError) and exfat/dirstate.go (resolveDir).
func TestReaderLimitErrorShapesAreUndecided(t *testing.T) {
	shapes := map[string]error{
		"fat-locate":    fmt.Errorf("%w: the search for the directory at cluster %d stopped after %d directories", filesys.ErrLimit, 77, 2),
		"exfat-resolve": fmt.Errorf("%w: the search for the directory at cluster %d stopped after %d directories", filesys.ErrLimit, 77, 2),
		"fat-cut":       fmt.Errorf("fat: %w: %w: directory at cluster %d: %s: %s", filesys.ErrLimit, filesys.ErrIncomplete, 5, "entries", "more than 65536 entries"),
	}
	for name, shapeErr := range shapes {
		t.Run(name, func(t *testing.T) {
			e, _ := newHookEnv(t, failByName(map[string]error{"a.bin": shapeErr}), delNode("/a.bin", pat(1, bs)), delNode("/ok.bin", pat(2, bs)))
			sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
			if err != nil || sum.SkippedBy["undecided"] != 1 || sum.Recovered != 1 {
				t.Fatalf("Recover = %v, SkippedBy %v, Recovered %d; want nil, one undecided, one recovered", err, sum.SkippedBy, sum.Recovered)
			}
		})
	}
}
