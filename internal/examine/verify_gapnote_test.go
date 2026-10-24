package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

const gapPrefix = "acquisition gaps: "

func gapVerify(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), examine.GapCheck())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func gapProblems(rep evidence.VerifyReport) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.Contains(p, gapPrefix) {
			out = append(out, p)
		}
	}
	return out
}

func noGapProblem(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	if ps := gapProblems(rep); len(ps) > 0 {
		t.Errorf("unexpected acquisition-gap problems: %q", ps)
	}
}

// requireGapProblemFor fails unless a gap problem names the artifact and contains substr.
func requireGapProblemFor(t *testing.T, rep evidence.VerifyReport, rec evidence.ManifestRecord, substr string) {
	t.Helper()
	for _, p := range gapProblems(rep) {
		if strings.Contains(p, fmt.Sprintf("artifact %q", rec.ID)) && strings.Contains(p, substr) {
			return
		}
	}
	t.Errorf("no gap problem for artifact %s containing %q in %q", rec.ID, substr, rep.Problems)
}

func noticeContaining(rep evidence.VerifyReport, substr string) bool {
	for _, n := range rep.Notices {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func derivedOfKind(recs []evidence.ManifestRecord, kinds ...string) []evidence.ManifestRecord {
	var out []evidence.ManifestRecord
	for _, r := range recs {
		for _, k := range kinds {
			if r.Source.Kind == k {
				out = append(out, r)
			}
		}
	}
	return out
}

func manifestOf(t *testing.T, c *evidence.Case) []evidence.ManifestRecord {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// gapEnv is an E01 case with one declared gap of three sectors inside /big.bin.
type gapEnv struct {
	c     *evidence.Case
	s     *examine.Session
	gapAt int64
}

func newGapEnv(t *testing.T) gapEnv {
	t.Helper()
	nodes, _, base := gapNodes()
	c := newCase(t)
	gapAt := int64(base) + 512
	s, _ := e01GapSession(t, c, nodes, gapAt, 3, true)
	return gapEnv{c: c, s: s, gapAt: gapAt}
}

// exact is the note the writer records for a run of 4096 bytes starting 512 before the gap.
func (e gapEnv) exact() *evidence.AcquisitionGapNote {
	return &evidence.AcquisitionGapNote{Bytes: 1536, Ranges: 1, First: []evidence.Run{{Offset: e.gapAt, Length: 1536}}}
}

func (e gapEnv) runs() []evidence.Run { return []evidence.Run{{Offset: e.gapAt - 512, Length: 4096}} }

// derive writes a crafted extract of e's parent; the data is zeros of the declared size.
func (e gapEnv) derive(t *testing.T, d evidence.Derivation) evidence.ManifestRecord {
	t.Helper()
	size := d.DeclaredSize
	if size == 0 {
		for _, r := range d.Runs {
			size += r.Length
		}
	}
	return evidencetest.AddDerived(t, e.c, e.s.Parent, make([]byte, max(size, 1)), evidence.Source{Kind: "extract", Derived: &d})
}

func TestGapRecomputeCleanOnRealProducers(t *testing.T) {
	damage := func(segs [][]byte) {
		for _, s := range segs {
			for _, sec := range ewftest.Sections(s) {
				if sec.Type == "error2" {
					s[sec.Offset+76+3] ^= 0xFF
				}
			}
		}
	}
	nodes, big, _ := gapNodes()
	nodes = append(nodes,
		fstest.Node{Path: "/inline.txt", Data: []byte("tiny"), Inline: true},
		fstest.Node{Path: "/empty.txt", Data: nil},
	)
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	for _, tc := range []struct {
		name   string
		mutate func([][]byte)
		gapAt  int64
		unallo bool
	}{
		{"complete overlap, clean, inline, empty and unallocated", nil, int64(base) + 512, true},
		{"incomplete extract cut by a bad chunk", corruptChunkAt(int64(base) + 3*e01Chunk), int64(base) + 512, false},
		{"damaged error2", damage, int64(base) + 512, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			s, _ := e01GapSessionMut(t, c, nodes, tc.gapAt, 3, true, tc.mutate)
			extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
			if tc.unallo {
				if _, err := s.ExportUnallocated(t.Context(), examine.UnallocOptions{Partition: -1}); err != nil {
					t.Fatal(err)
				}
			}
			recs := derivedOfKind(manifestOf(t, c), "extract", "unallocated")
			if len(recs) < 3 {
				t.Fatalf("only %d extract or unallocated artifacts", len(recs))
			}
			rep := gapVerify(t, c)
			noGapProblem(t, rep)
			if rep.GapNotesRecomputed != len(recs) {
				t.Errorf("GapNotesRecomputed = %d, want %d (notices %q)", rep.GapNotesRecomputed, len(recs), rep.Notices)
			}
			if noticeContaining(rep, "not recomputable") {
				t.Errorf("a real producer needed a not-recomputable notice: %q", rep.Notices)
			}
		})
	}
}

func TestGapRecomputeFlagsForgedNote(t *testing.T) {
	e := newGapEnv(t)
	good := e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096, AcquisitionGaps: e.exact()})
	plus := func(f func(n *evidence.AcquisitionGapNote)) *evidence.AcquisitionGapNote {
		n := e.exact()
		f(n)
		return n
	}
	forged := map[string]evidence.ManifestRecord{
		"bytes+1":      e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096, AcquisitionGaps: plus(func(n *evidence.AcquisitionGapNote) { n.Bytes++ })}),
		"ranges+1":     e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096, AcquisitionGaps: plus(func(n *evidence.AcquisitionGapNote) { n.Ranges++ })}),
		"unknown set":  e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096, AcquisitionGaps: plus(func(n *evidence.AcquisitionGapNote) { n.Unknown = true })}),
		"note removed": e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096}),
		// overlapping runs make the true count unknown; a note that says "exact" is a forgery
		"unknown cleared": e.derive(t, evidence.Derivation{
			Runs:         []evidence.Run{{Offset: e.gapAt - 512, Length: 4096}, {Offset: e.gapAt, Length: 512}},
			DeclaredSize: 4608, AcquisitionGaps: e.exact(),
		}),
	}
	raw := evidencetest.AddImage(t, e.c, bytes.Repeat([]byte{7}, 64<<10))
	forged["note on a raw parent"] = evidencetest.AddDerived(t, e.c, raw, make([]byte, 512), evidence.Source{Kind: "extract", Derived: &evidence.Derivation{
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, DeclaredSize: 512,
		AcquisitionGaps: &evidence.AcquisitionGapNote{Bytes: 512, Ranges: 1, First: []evidence.Run{{Offset: 0, Length: 512}}},
	}})
	rep := gapVerify(t, e.c)
	for name, rec := range forged {
		want := "recomputed from the container and its runs"
		if name == "note on a raw parent" {
			want = "records no acquisition errors"
		}
		t.Run(name, func(t *testing.T) { requireGapProblemFor(t, rep, rec, want) })
	}
	for _, p := range gapProblems(rep) {
		if strings.Contains(p, fmt.Sprintf("artifact %q", good.ID)) {
			t.Errorf("the honest note is a problem: %s", p)
		}
	}
	if rep.OK() {
		t.Error("a forged gap note must make the verify fail")
	}
	if rep.GapNotesRecomputed != len(forged)+1 {
		t.Errorf("GapNotesRecomputed = %d, want %d", rep.GapNotesRecomputed, len(forged)+1)
	}
}

func TestGapRecomputeIncompleteWithoutRunsNotices(t *testing.T) {
	e := newGapEnv(t)
	d := evidence.Derivation{DeclaredSize: 4096, AcquisitionGaps: e.exact()}
	rec := evidencetest.AddDerivedAt(t, e.c, e.s.Parent, "derived/cut.bin", make([]byte, 2048),
		evidence.Source{Kind: "extract", Derived: &d}, errors.New("cut: the runs of the kept bytes are not recorded"))
	if !rec.Incomplete {
		t.Fatal("fixture is not incomplete")
	}
	rep := gapVerify(t, e.c)
	noGapProblem(t, rep)
	if !noticeContaining(rep, gapPrefix+"not recomputable: the runs of the kept bytes are not recorded") {
		t.Errorf("no K21 notice in %q", rep.Notices)
	}
	if rep.GapNotesRecomputed != 0 {
		t.Errorf("GapNotesRecomputed = %d, nothing was compared", rep.GapNotesRecomputed)
	}
}

func TestGapRecomputeDeletedSidecarIsProblem(t *testing.T) {
	e := newGapEnv(t)
	sidecar := func(name string) evidence.ManifestRecord {
		line, _ := json.Marshal(e.runs()[0])
		return evidencetest.AddDerivedAt(t, e.c, e.s.Parent, "derived/"+name+".runs.jsonl", append(line, '\n'),
			evidence.Source{Kind: "runs", Derived: &evidence.Derivation{}}, nil)
	}
	scComplete, scIncomplete := sidecar("complete"), sidecar("incomplete")
	complete := e.derive(t, evidence.Derivation{RunsArtifact: scComplete.ID, DeclaredSize: 4096, AcquisitionGaps: e.exact()})
	incomplete := evidencetest.AddDerivedAt(t, e.c, e.s.Parent, "derived/incomplete.bin", make([]byte, 2048),
		evidence.Source{Kind: "extract", Derived: &evidence.Derivation{RunsArtifact: scIncomplete.ID, DeclaredSize: 4096, AcquisitionGaps: e.exact()}},
		errors.New("cut"))
	rep := gapVerify(t, e.c)
	noGapProblem(t, rep)
	if rep.GapNotesRecomputed != 2 {
		t.Fatalf("with the sidecars present GapNotesRecomputed = %d, want 2", rep.GapNotesRecomputed)
	}
	evidencetest.RemoveArtifactFile(t, e.c.Dir, scComplete)
	evidencetest.RemoveArtifactFile(t, e.c.Dir, scIncomplete)
	rep = gapVerify(t, e.c)
	requireGapProblemFor(t, rep, complete, gapPrefix+"runs unreadable")
	requireGapProblemFor(t, rep, incomplete, gapPrefix+"runs unreadable")
	if noticeContaining(rep, "not recomputable: the runs") {
		t.Errorf("a deleted sidecar must be a problem, never the not-recorded notice: %q", rep.Notices)
	}
}

// wholeFileGap is a 256 KiB file lying wholly inside the declared gap: a copy cut after its first buffer
// leaves an artifact much smaller than the file, so the clamp "Bytes > size" bites with the wrong size.
func wholeFileGap() (nodes []fstest.Node, base int, size int) {
	size = 8 * (32 << 10)
	data := pattern(size, 3)
	nodes = []fstest.Node{{Path: "/big.bin", Data: data}}
	base = bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), data[:64])
	return nodes, base, size
}

// requireCutExtract checks the shape of an extract cut mid-copy and that R10 recomputes it with the writer's
// size (declared_size), which differs from the size of the kept bytes.
func requireCutExtract(t *testing.T, c *evidence.Case, s *examine.Session, size int) {
	t.Helper()
	recs := derivedOfKind(manifestOf(t, c), "extract")
	if len(recs) != 1 {
		t.Fatalf("%d extracts, want 1", len(recs))
	}
	rec := recs[0]
	d := rec.Source.Derived
	if !rec.Incomplete || rec.Size <= 0 || rec.Size >= int64(size) || d.DeclaredSize != int64(size) {
		t.Fatalf("incomplete %t size %d declared_size %d, want an incomplete artifact of 0 < size < %d with declared_size %d",
			rec.Incomplete, rec.Size, d.DeclaredSize, size, size)
	}
	if d.AcquisitionGaps == nil || d.AcquisitionGaps.Unknown || d.AcquisitionGaps.Bytes != int64(size) {
		t.Fatalf("recorded note = %+v, want the whole file in the gap, exact", d.AcquisitionGaps)
	}
	rep := gapVerify(t, c)
	noGapProblem(t, rep)
	if rep.GapNotesRecomputed != 1 {
		t.Errorf("GapNotesRecomputed = %d, want 1", rep.GapNotesRecomputed)
	}
	oc, err := examine.OpenContainer(c, s.Parent.ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oc.Close() }()
	g := oc.Image.(image.GapReporter).AcquisitionGaps()
	runs, err := c.DerivedRuns(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	right := examine.GapNoteFor(g, runs, true, d.DeclaredSize)
	wrong := examine.GapNoteFor(g, runs, true, rec.Size)
	if !reflect.DeepEqual(right, d.AcquisitionGaps) {
		t.Errorf("gapNoteFor(declared size) = %+v, recorded %+v", right, d.AcquisitionGaps)
	}
	if reflect.DeepEqual(wrong, right) {
		t.Errorf("recomputing with the kept size gives the same note %+v: the test does not show the clamp", wrong)
	}
}

func TestGapRecomputeIncompleteFromCancel(t *testing.T) {
	nodes, base, size := wholeFileGap()
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, int64(base), int64(size/512), true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.Extract(ctx, examine.ExtractOptions{
		Partition: -1, Paths: []string{"/big.bin"},
		Progress: func(done, _ int64) {
			if done > 0 {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	requireCutExtract(t, c, s, size)
}

// eofFile ends a read with io.ErrUnexpectedEOF once it reaches past after.
type eofFile struct {
	filesys.File
	after int64
}

func (f eofFile) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.after {
		n := 0
		if off < f.after {
			n, _ = f.File.ReadAt(p[:f.after-off], off)
		}
		return n, io.ErrUnexpectedEOF
	}
	return f.File.ReadAt(p, off)
}

func hookOpts(h hookFS) examine.Options {
	return examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return hookedFS{FileSystem: fsys, h: h}, nil
		},
	}}}
}

func TestGapRecomputeIncompleteFromUnexpectedEOF(t *testing.T) {
	nodes, base, size := wholeFileGap()
	c := newCase(t)
	opts := hookOpts(hookFS{mapFile: func(_ filesys.Entry, f filesys.File) filesys.File { return eofFile{File: f, after: 40000} }})
	s, _ := e01GapSessionOpts(t, c, nodes, int64(base), int64(size/512), true, nil, opts)
	if _, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	requireCutExtract(t, c, s, size)
}

func TestExtractRecordsDeclaredSize(t *testing.T) {
	nodes, _, base := gapNodes()
	nodes = append(nodes, fstest.Node{Path: "/empty.txt"})
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, int64(base)+512, 3, true)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	sizes := map[string]int64{}
	for _, n := range nodes {
		sizes[n.Path] = int64(len(n.Data))
	}
	audited := map[string]map[string]any{}
	for _, e := range auditByAction(t, c, "artifact.create") {
		audited[e.Details["id"].(string)] = e.Details["source"].(map[string]any)
	}
	var seen int
	for _, r := range sum.Artifacts {
		if r.Source.Kind != "extract" {
			continue
		}
		seen++
		want := sizes[r.Source.RemotePath]
		if got := r.Source.Derived.DeclaredSize; got != want {
			t.Errorf("%s: declared_size %d, want the source size %d", r.Source.RemotePath, got, want)
		}
		dv, _ := audited[r.ID]["derived"].(map[string]any)
		got, has := dv["declared_size"]
		if want > 0 && fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: the artifact.create source carries declared_size %v, want %d", r.Source.RemotePath, got, want)
		}
		if want == 0 && has {
			t.Errorf("%s: a 0-byte file records declared_size %v", r.Source.RemotePath, got)
		}
	}
	if seen < 2 {
		t.Fatalf("only %d extracts", seen)
	}
}

func TestGapRecomputeLegacyExtractWithoutNoteNotices(t *testing.T) {
	e := newGapEnv(t)
	legacy := e.derive(t, evidence.Derivation{Runs: e.runs()}) // no declared_size, no note, runs over the gap
	withSize := e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096})
	rep := gapVerify(t, e.c)
	if !noticeContaining(rep, gapPrefix+"gap note not recomputable (recorded before gap notes)") {
		t.Errorf("no legacy notice in %q", rep.Notices)
	}
	for _, p := range gapProblems(rep) {
		if strings.Contains(p, fmt.Sprintf("artifact %q", legacy.ID)) {
			t.Errorf("a legacy entry without a note is a problem: %s", p)
		}
	}
	requireGapProblemFor(t, rep, withSize, "the artifact records none")
	if rep.GapNotesRecomputed != 1 {
		t.Errorf("GapNotesRecomputed = %d, want 1 (the entry with declared_size only)", rep.GapNotesRecomputed)
	}
}

func TestGapRecomputeLegacyExtractWithNoteIsCompared(t *testing.T) {
	e := newGapEnv(t)
	right := e.derive(t, evidence.Derivation{Runs: e.runs(), AcquisitionGaps: e.exact()})
	bad := e.exact()
	bad.Bytes++
	forged := e.derive(t, evidence.Derivation{Runs: e.runs(), AcquisitionGaps: bad})
	rep := gapVerify(t, e.c)
	requireGapProblemFor(t, rep, forged, "recomputed from the container and its runs")
	for _, p := range gapProblems(rep) {
		if strings.Contains(p, fmt.Sprintf("artifact %q", right.ID)) {
			t.Errorf("the honest legacy note is a problem: %s", p)
		}
	}
	if rep.GapNotesRecomputed != 2 {
		t.Errorf("GapNotesRecomputed = %d, want 2", rep.GapNotesRecomputed)
	}
}

func TestGapNoticeWhenNotComposed(t *testing.T) {
	e := newGapEnv(t)
	e.derive(t, evidence.Derivation{Runs: e.runs(), DeclaredSize: 4096, AcquisitionGaps: e.exact()})
	rep, err := e.c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	noGapProblem(t, rep)
	if !noticeContaining(rep, "1 extract or unallocated artifact(s): acquisition-gap notes shape-checked only, not recomputed (this verify did not compose check R10)") {
		t.Errorf("no not-composed notice in %q", rep.Notices)
	}
	if rep.GapNotesRecomputed != 0 {
		t.Errorf("GapNotesRecomputed = %d without the check", rep.GapNotesRecomputed)
	}
	rep = gapVerify(t, e.c)
	if noticeContaining(rep, "shape-checked only") {
		t.Errorf("notice although R10 ran: %q", rep.Notices)
	}
}

// A note computed under the work bound is Unknown; the recompute meets the same bound, so the honest
// record is not flagged.
func TestGapRecomputeBoundedWork(t *testing.T) {
	defer examine.SetMaxGapWork(3)()
	const frags = 8
	data := pattern(frags*blk, 3)
	nodes := []fstest.Node{{Path: "/frag.bin", Data: data, Fragments: frags}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), data[:64])
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, int64(base), 2*frags, true) // fragments sit one block apart: all inside the gap
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/frag.bin"}})
	recs := derivedOfKind(sum.Artifacts, "extract")
	if len(recs) != 1 {
		t.Fatalf("%d extracts", len(recs))
	}
	if n := recs[0].Source.Derived.AcquisitionGaps; n == nil || !n.Unknown {
		t.Fatalf("note = %+v, want unknown past the work bound", n)
	}
	rep := gapVerify(t, c)
	noGapProblem(t, rep)
	if rep.GapNotesRecomputed != 1 {
		t.Errorf("GapNotesRecomputed = %d, want 1", rep.GapNotesRecomputed)
	}
}
