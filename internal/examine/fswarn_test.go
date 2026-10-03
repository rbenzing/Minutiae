package examine_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// warnFS wraps an MTFS filesystem whose Info().Warnings grow while it works:
// Unallocated appends unalloc, Open appends what onOpen returns.
type warnFS struct {
	filesys.FileSystem
	warnings *[]string
	unalloc  []string
	onOpen   func(filesys.Entry) []string
}

func (f warnFS) Info() filesys.Info {
	i := f.FileSystem.Info()
	i.Warnings = append([]string(nil), *f.warnings...)
	return i
}

func (f warnFS) Unallocated() ([]filesys.Run, error) {
	*f.warnings = append(*f.warnings, f.unalloc...)
	return f.FileSystem.Unallocated()
}

func (f warnFS) Open(e filesys.Entry) (filesys.File, error) {
	if f.onOpen != nil {
		*f.warnings = append(*f.warnings, f.onOpen(e)...)
	}
	return f.FileSystem.Open(e)
}

// warnSession opens an MTFS image through a warnFS that starts with the open
// warnings.
func warnSession(t *testing.T, c *evidence.Case, open []string, mk func(w *[]string) warnFS, nodes ...fstest.Node) *examine.Session {
	t.Helper()
	recs := importImage(t, c, disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), 1)
	warnings := append([]string(nil), open...)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			w := mk(&warnings)
			w.FileSystem = fsys
			return w, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fsWarnings returns the analysis.warning entries that carry a source, as
// source -> warning texts in audit order.
func fsWarnings(t *testing.T, c *evidence.Case) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, e := range auditByAction(t, c, "analysis.warning") {
		src, _ := e.Details["source"].(string)
		if src == "" {
			continue
		}
		w, _ := e.Details["warning"].(string)
		out[src] = append(out[src], w)
	}
	return out
}

func TestUnallocForwardsFilesystemWarnings(t *testing.T) {
	c := newCase(t)
	s := warnSession(t, c, []string{"image truncated"}, func(w *[]string) warnFS {
		return warnFS{warnings: w, unalloc: []string{"group 3 skipped, not reported as free", "image truncated"}}
	})
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	got := fsWarnings(t, c)
	if want := []string{"image truncated"}; !slices.Equal(got["filesystem-open"], want) {
		t.Errorf("filesystem-open warnings = %q, want %q", got["filesystem-open"], want)
	}
	if want := []string{"group 3 skipped, not reported as free"}; !slices.Equal(got["filesystem"], want) {
		t.Errorf("filesystem warnings = %q, want %q (the open-time text is not repeated)", got["filesystem"], want)
	}
	if sum.Skipped != 1 || len(sum.Warnings) != 1 || sum.Warnings[0].Reason != "group 3 skipped, not reported as free" {
		t.Errorf("summary skipped=%d warnings=%+v, want the one new filesystem warning", sum.Skipped, sum.Warnings)
	}
	// The open-time state is recorded inside the analysis, after its start.
	var started bool
	for _, e := range auditEntries(t, c) {
		switch {
		case e.Action == "analysis.start":
			started = true
		case e.Action == "analysis.warning" && e.Details["source"] == "filesystem-open":
			if !started || e.Details["analysis_id"] != sum.AnalysisID {
				t.Errorf("open warning entry %+v outside its analysis", e.Details)
			}
		}
	}
	verifyOK(t, c)
}

func TestExtractForwardsFilesystemWarningsOnce(t *testing.T) {
	c := newCase(t)
	s := warnSession(t, c, []string{"checksum mismatch in group 0"}, func(w *[]string) warnFS {
		return warnFS{warnings: w, onOpen: func(e filesys.Entry) []string {
			return []string{"file data pointer into filesystem metadata: " + e.Name, "shared anomaly"}
		}}
	},
		fstest.Node{Path: "/a.txt", Data: []byte("alpha")},
		fstest.Node{Path: "/b.txt", Data: []byte("bravo")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt", "/b.txt"}})
	if sum.Files != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	got := fsWarnings(t, c)
	if want := []string{"checksum mismatch in group 0"}; !slices.Equal(got["filesystem-open"], want) {
		t.Errorf("filesystem-open warnings = %q, want %q", got["filesystem-open"], want)
	}
	want := []string{
		"file data pointer into filesystem metadata: a.txt", "shared anomaly",
		"file data pointer into filesystem metadata: b.txt",
	}
	if !slices.Equal(got["filesystem"], want) {
		t.Errorf("filesystem warnings = %q, want %q", got["filesystem"], want)
	}
	if sum.Skipped != 3 || len(sum.Warnings) != 3 {
		t.Errorf("summary skipped=%d warnings=%+v, want the 3 new ones", sum.Skipped, sum.Warnings)
	}
	// The first file's warning is recorded before the second file is written.
	var warnSeq, bSeq int64
	for _, e := range auditEntries(t, c) {
		if e.Action == "analysis.warning" && e.Details["warning"] == "file data pointer into filesystem metadata: a.txt" {
			warnSeq = e.Seq
		}
		if p, _ := e.Details["path"].(string); e.Action == "artifact.create" && strings.HasSuffix(p, "/b.txt") {
			bSeq = e.Seq
		}
	}
	if warnSeq == 0 || bSeq == 0 || warnSeq > bSeq {
		t.Errorf("warning seq %d, second artifact seq %d: want the warning first", warnSeq, bSeq)
	}

	// A second analysis starts from the state the first left: those warnings
	// are open-time context now and none is new.
	before := len(auditByAction(t, c, "analysis.warning"))
	sum2 := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	if sum2.Skipped != 0 {
		t.Errorf("second analysis skipped = %d, want 0 (the filesystem only repeats itself)", sum2.Skipped)
	}
	if n := len(auditByAction(t, c, "analysis.warning")) - before; n != 4 {
		t.Errorf("second analysis wrote %d warning entries, want the 4 distinct open-time ones", n)
	}
}

func TestFilesystemWarningsSummaryIsCappedAuditIsNot(t *testing.T) {
	c := newCase(t)
	const n = 130
	many := make([]string, n)
	for i := range many {
		many[i] = fmt.Sprintf("anomaly %d", i)
	}
	s := warnSession(t, c, nil, func(w *[]string) warnFS { return warnFS{warnings: w, unalloc: many} })
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != n || len(sum.Warnings) != 100 {
		t.Errorf("skipped=%d len(Warnings)=%d, want %d and 100", sum.Skipped, len(sum.Warnings), n)
	}
	if got := len(fsWarnings(t, c)["filesystem"]); got != n {
		t.Errorf("audit has %d filesystem warnings, want all %d", got, n)
	}
}

func TestFilesystemWarningsReachAuditWhenAnalysisFails(t *testing.T) {
	c := newCase(t)
	s := warnSession(t, c, nil, func(w *[]string) warnFS {
		return warnFS{warnings: w, onOpen: func(filesys.Entry) []string { return []string{"anomaly before the abort"} }}
	}, fstest.Node{Path: "/a.txt", Data: pattern(4*blk, 3)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.Extract(ctx, examine.ExtractOptions{
		Partition: -1, Paths: []string{"/a.txt"},
		Progress: func(int64, int64) { cancel() }, // cancelled while the first file streams
	})
	if err == nil {
		t.Fatal("cancelled extract succeeded")
	}
	if want := []string{"anomaly before the abort"}; !slices.Equal(fsWarnings(t, c)["filesystem"], want) {
		t.Errorf("filesystem warnings = %q, want %q", fsWarnings(t, c)["filesystem"], want)
	}
	if len(auditByAction(t, c, "analysis.error")) != 1 {
		t.Error("no analysis.error entry")
	}
}
