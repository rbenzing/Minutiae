package examine_test

import (
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func budgetNodes() []fstest.Node {
	return []fstest.Node{
		{Path: "/a.bin", Data: pattern(3*blk, 1)},
		{Path: "/b.bin", Data: pattern(3*blk, 2)},
		{Path: "/c.bin", Data: pattern(3*blk, 3)},
	}
}

func TestExtractOutputBudgetStopsTypedAndReported(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, budgetNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxBytes: int64(5 * blk)})
	if sum.OutputLimit != "max-bytes" || sum.Files != 1 || sum.Bytes != int64(3*blk) {
		t.Fatalf("summary = %+v; want 1 file (a.bin), 3 blocks, OutputLimit max-bytes", sum)
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 1 || ws[0].Details["path"] != "/b.bin" {
		t.Fatalf("warnings = %+v; want exactly one, naming the first refused file /b.bin", ws)
	}
	ends := auditByAction(t, c, "analysis.end")
	if len(ends) != 1 || ends[0].Details["limit_reached"] != "max-bytes" {
		t.Fatalf("analysis.end = %+v; want limit_reached max-bytes", ends)
	}
	verifyOK(t, c)
}

func TestExtractOutputBudgetMaxFiles(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, budgetNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxFiles: 2})
	if sum.OutputLimit != "max-files" || sum.Files != 2 {
		t.Fatalf("summary = %+v; want 2 files and OutputLimit max-files", sum)
	}
}

func TestExtractWithinTheBudgetHasNoLimit(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, budgetNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.OutputLimit != "" || sum.Files != 3 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestExtractRejectsNegativeLimit(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, budgetNodes()...)
	before := len(auditByAction(t, c, "analysis.start"))
	for _, o := range []examine.ExtractOptions{
		{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxBytes: -1},
		{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxFiles: -1},
	} {
		sum, err := s.Extract(t.Context(), o)
		if !errors.Is(err, examine.ErrInvalidLimit) || sum.AnalysisID != "" {
			t.Fatalf("Extract(%+v) = %+v, %v; want ErrInvalidLimit before any analysis", o, sum, err)
		}
	}
	if got := len(auditByAction(t, c, "analysis.start")); got != before {
		t.Fatalf("a rejected limit audited %d analysis.start entries", got-before)
	}
}

func TestExtractSkipsFileWhoseDataRunsOverlap(t *testing.T) {
	const huge = int64(1) << 50
	for name, f := range map[string]sparseFile{
		"two runs over the same data": {size: huge, runs: []filesys.Run{{Offset: 0, Length: huge / 2}, {Offset: 0, Length: huge / 2}}},
		"data run lengths overflow":   {size: 0, runs: []filesys.Run{{Offset: 0, Length: math.MaxInt64}, {Offset: 0, Length: math.MaxInt64}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCase(t)
			s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, file filesys.File) filesys.File {
				if e.Name != "bomb.bin" {
					return file
				}
				f.File = file
				return f
			}}, 0,
				fstest.Node{Path: "/bomb.bin", Data: []byte("hello")},
				fstest.Node{Path: "/ok.txt", Data: []byte("fine")})
			sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/bomb.bin", "/ok.txt"}})
			if sum.Files != 1 || sum.Skipped != 1 || sum.Bytes != 4 {
				t.Fatalf("summary = %+v", sum)
			}
			ws := auditByAction(t, c, "analysis.warning")
			if len(ws) != 1 || ws[0].Details["reason"] != "data runs exceed partition length; not extracted" {
				t.Fatalf("warnings = %+v", ws)
			}
			verifyOK(t, c)
		})
	}
}

// brokenFile reads as corrupt from the first byte.
type brokenFile struct{ filesys.File }

func (brokenFile) ReadAt([]byte, int64) (int, error) { return 0, filesys.ErrCorrupt }

// X39: bytes admitted for a file that then could not be read are given back, so a
// later file that fits the output limit is still extracted.
func TestExtractBudgetRefundsAFileThatCouldNotBeRead(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, file filesys.File) filesys.File {
		if e.Name == "a.bin" {
			return brokenFile{file}
		}
		return file
	}}, 0, budgetNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxBytes: int64(5 * blk)})
	// a.bin is kept empty and incomplete and skipped (its 3 blocks are given back),
	// b.bin fits the 5 blocks, c.bin does not: the run ends there.
	if sum.OutputLimit != "max-bytes" || sum.Files != 1 || sum.Bytes != int64(3*blk) || len(sum.Warnings) != 2 ||
		sum.Warnings[0].Path != "/a.bin" || sum.Warnings[1].Path != "/c.bin" {
		t.Fatalf("summary = %+v; want b.bin extracted and the limit reached at c.bin", sum)
	}
}
