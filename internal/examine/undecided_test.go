package examine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// undecidedFS makes a lookup under /cf and the Open of /top.txt answer filesys.ErrUndecided, as a casefold
// directory with a name of an unmeasured shape does.
type undecidedFS struct{ filesys.FileSystem }

func (u undecidedFS) Lookup(p string) (filesys.Entry, error) {
	if strings.HasPrefix(p, "/cf") {
		return filesys.Entry{}, fmt.Errorf("fake: %w: lookup of %q", filesys.ErrUndecided, p)
	}
	return u.FileSystem.Lookup(p)
}

func (u undecidedFS) Open(e filesys.Entry) (filesys.File, error) {
	if e.Name == "top.txt" {
		return nil, fmt.Errorf("fake: %w: open of %q", filesys.ErrUndecided, e.Name)
	}
	return u.FileSystem.Open(e)
}

func undecidedSession(t *testing.T, c *evidence.Case) *examine.Session {
	t.Helper()
	recs := importImage(t, c, disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: []fstest.Node{
		{Path: "/top.txt", Data: []byte("top")},
		{Path: "/docs/a.txt", Data: []byte("a")},
	}})), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return undecidedFS{fsys}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Ruling Z45: an undecided lookup in a batch path ends the run with the typed error. It is not skipped as
// "not found" (examine's skippable) and not counted as a skipped entry.
func TestExtractOfAnUndecidedLookupFailsTyped(t *testing.T) {
	c := newCase(t)
	s := undecidedSession(t, c)
	sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Recursive: true, Paths: []string{"/docs", "/cf/x"}})
	if !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("Extract = %v, want an error wrapping ErrUndecided and not ErrNotFound", err)
	}
	if sum.Skipped != 0 {
		t.Errorf("the undecided lookup was counted as a skip: %+v", sum)
	}
}

// ... and so is an undecided Open of a file met while extracting a tree.
func TestExtractOfAnUndecidedOpenIsNotSkipped(t *testing.T) {
	c := newCase(t)
	s := undecidedSession(t, c)
	sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Recursive: true, Paths: []string{"/"}})
	if !errors.Is(err, filesys.ErrUndecided) {
		t.Fatalf("Extract = %v, %+v; want an error wrapping ErrUndecided (not skipped with a warning)", err, sum)
	}
}

func (u undecidedFS) Underlying() filesys.FileSystem { return u.FileSystem }

// Ruling Z48: a path lookup that ends an extract before the analysis starts, because the path is undecided
// or not found, is audited: an analysis.start and an analysis.error that name the path and the reason.
// Nothing is extracted.
func TestExtractPathLookupFailureIsAudited(t *testing.T) {
	for _, c := range []struct {
		path, reason string
		want         error
	}{
		{"/cf/x", "undecided", filesys.ErrUndecided},
		{"/nope", "not-found", filesys.ErrNotFound},
	} {
		t.Run(c.reason, func(t *testing.T) {
			cs := newCase(t)
			s := undecidedSession(t, cs)
			sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Recursive: true, Paths: []string{"/docs", c.path}})
			if !errors.Is(err, c.want) {
				t.Fatalf("Extract = %v, want %v", err, c.want)
			}
			if sum.Files != 0 || len(sum.Artifacts) != 0 {
				t.Errorf("something was extracted before the failing lookup: %+v", sum)
			}
			errs := auditByAction(t, cs, "analysis.error")
			if len(errs) != 1 {
				t.Fatalf("%d analysis.error entries, want 1", len(errs))
			}
			d := errs[0].Details
			if got, _ := d["path"].(string); got != c.path {
				t.Errorf("analysis.error path = %q, want %q", got, c.path)
			}
			if got, _ := d["reason"].(string); got != c.reason {
				t.Errorf("analysis.error reason = %q, want %q", got, c.reason)
			}
			if msg, _ := d["error"].(string); !strings.Contains(msg, c.path) {
				t.Errorf("analysis.error does not name the path: %q", msg)
			}
			if len(auditByAction(t, cs, "analysis.start")) != 1 {
				t.Error("no analysis.start before the analysis.error")
			}
			if len(auditByAction(t, cs, "analysis.end")) != 0 {
				t.Error("an analysis.end for a failed lookup")
			}
		})
	}
}

// The real recovery run plans inside its analysis, so a ref that fails to resolve is audited there; the
// dry run (PlanRecovery) stays read-only and writes nothing.
func TestRecoverPathLookupFailureIsAudited(t *testing.T) {
	for _, c := range []struct {
		path, reason string
		want         error
	}{
		{"/cf/x", "undecided", filesys.ErrUndecided},
		{"/nope", "not-found", filesys.ErrNotFound},
	} {
		t.Run(c.reason, func(t *testing.T) {
			cs := newCase(t)
			s := undecidedSession(t, cs)
			_, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{c.path}})
			if !errors.Is(err, c.want) {
				t.Fatalf("Recover = %v, want %v", err, c.want)
			}
			errs := auditByAction(t, cs, "analysis.error")
			if len(errs) != 1 {
				t.Fatalf("%d analysis.error entries, want 1", len(errs))
			}
			if msg, _ := errs[0].Details["error"].(string); !strings.Contains(msg, c.path) {
				t.Errorf("analysis.error does not name the path: %q", msg)
			}
			before := len(auditEntries(t, cs))
			if _, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{c.path}}); !errors.Is(err, c.want) {
				t.Errorf("PlanRecovery = %v, want %v", err, c.want)
			}
			if after := len(auditEntries(t, cs)); after != before {
				t.Errorf("the dry run appended %d audit entries", after-before)
			}
		})
	}
}
