package examine_test

import (
	"errors"
	"io/fs"
	"math"
	"strings"
	"syscall"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
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

// releaseNodes is two 3-block files under a 4-block limit: b.bin fits only when a.bin's
// admitted 3 blocks were given back.
func releaseNodes() []fstest.Node {
	return []fstest.Node{
		{Path: "/a.bin", Data: pattern(3*blk, 1)},
		{Path: "/b.bin", Data: pattern(3*blk, 2)},
	}
}

// X44: each of the four skips before any byte is written gives the admitted size back.
func TestExtractBudgetRefundsEachNothingWrittenSkip(t *testing.T) {
	limit := examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxBytes: int64(4 * blk)}
	check := func(t *testing.T, c *evidence.Case, s *examine.Session, wantPath, wantWarn string) {
		t.Helper()
		sum := extractAll(t, s, limit)
		if sum.OutputLimit != "" || sum.Files != 1 || sum.Bytes != int64(3*blk) || sum.Skipped != 1 {
			t.Fatalf("summary = %+v; want a.bin skipped, its charge given back, b.bin extracted, no limit", sum)
		}
		if len(sum.Warnings) != 1 || sum.Warnings[0].Path != wantPath || !strings.Contains(sum.Warnings[0].Reason, wantWarn) {
			t.Fatalf("warnings = %+v; want one for %s containing %q", sum.Warnings, wantPath, wantWarn)
		}
		verifyOK(t, c)
	}
	t.Run("no usable local name", func(t *testing.T) {
		c := newCase(t)
		s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
			if e.Name == "a.bin" {
				e.Name = "C:" // a drive-letter name the case cannot hold
			}
			return e
		}}, 0, releaseNodes()...)
		check(t, c, s, "/C:", "no usable local name")
	})
	t.Run("operating system rejects the name", func(t *testing.T) {
		c := newCase(t)
		s, _ := session(t, c, 0, releaseNodes()...)
		s.SetNewArtifact(func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
			if strings.HasSuffix(rel, "/a.bin") {
				return nil, &fs.PathError{Op: "open", Path: rel, Err: syscall.ENAMETOOLONG}
			}
			return c.NewArtifact(deviceID, acqID, rel, src)
		})
		check(t, c, s, "/a.bin", "not extracted")
	})
	t.Run("no free local name", func(t *testing.T) {
		c := newCase(t)
		s, _ := session(t, c, 0, releaseNodes()...)
		s.SetNewArtifact(func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
			if strings.HasSuffix(rel, "/a.bin") || (strings.Contains(rel, "/a~") && strings.HasSuffix(rel, ".bin")) {
				return nil, evidence.ErrArtifactExists
			}
			return c.NewArtifact(deviceID, acqID, rel, src)
		})
		check(t, c, s, "/a.bin", "no free local name")
	})
	t.Run("no usable local directory name", func(t *testing.T) {
		c := newCase(t)
		s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
			if e.Name == "d" {
				e.Name = "C:" // a drive-letter directory name the case cannot hold
			}
			return e
		}}, 0, fstest.Node{Path: "/d/a.bin", Data: pattern(3*blk, 1)}, fstest.Node{Path: "/e.bin", Data: pattern(3*blk, 2)})
		check(t, c, s, "/C:/a.bin", "no usable local directory name")
	})
}

// X44: a file whose read stops part way keeps its partial bytes, flagged incomplete, and is
// charged for the bytes written only: the rest of its admitted size is given back.
func TestExtractBudgetTrimsAPartWrittenFile(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, file filesys.File) filesys.File {
		if e.Name == "a.bin" {
			return failingFile{File: file, after: blk}
		}
		return file
	}}, 0, budgetNodes()...)
	// a.bin (3 blocks admitted, 1 written) + b.bin (3) = 4 blocks fit 5; c.bin (3) does not.
	// Untrimmed, a.bin would stay charged 3 blocks and b.bin would already be refused.
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, MaxBytes: int64(5 * blk)})
	if sum.OutputLimit != "max-bytes" || sum.Files != 1 || len(sum.Artifacts) != 2 || sum.Bytes != int64(3*blk) {
		t.Fatalf("summary = %+v; want b.bin extracted besides the part-written a.bin, limit at c.bin", sum)
	}
	part := sum.Artifacts[0]
	if part.Source.RemotePath != "/a.bin" || part.Size != blk || !part.Incomplete {
		t.Fatalf("a.bin artifact = %+v; want %d bytes, flagged incomplete", part, blk)
	}
	if last := sum.Warnings[len(sum.Warnings)-1]; last.Path != "/c.bin" {
		t.Fatalf("warnings = %+v; want the limit at /c.bin", sum.Warnings)
	}
	verifyOK(t, c)
}
