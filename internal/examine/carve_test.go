package examine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// The carve writer tests live inside the package, beside the scope tests: they share carveEnv and its hook.

func carvePad(b []byte) []byte {
	out := make([]byte, (len(b)+carveBS-1)/carveBS*carveBS)
	copy(out, b)
	return out
}

func carveJPEG() []byte { return carvetest.JPEG(carvetest.JPEGOpts{}) }
func carvePNG() []byte  { return carvetest.PNG(carvetest.PNGOpts{}) }

func carveDeleted(path string, obj []byte) fstest.Node {
	return fstest.Node{Path: path, Data: carvePad(obj), Deleted: true, Freed: true}
}

// carveImageOff is the image offset of the first block of path (the GPT partition starts at carveFirstLBA).
func carveImageOff(t *testing.T, path string, nodes ...fstest.Node) int64 {
	t.Helper()
	return int64(carveFirstLBA)*512 + carveLayout(t, path, nodes...)[0].Offset
}

// carveStdNodes holds a JPEG and a PNG in freed blocks, between live files.
func carveStdNodes(jpg, png []byte) []fstest.Node {
	return []fstest.Node{
		{Path: "/a.bin", Data: carveFill(1, 8192)},
		carveDeleted("/del1.jpg", jpg),
		{Path: "/b.bin", Data: carveFill(2, 4096)},
		carveDeleted("/del2.png", png),
	}
}

func (e *carveEnv) carve(t *testing.T, o CarveOptions) CarveSummary {
	t.Helper()
	if o.MinConfidence == 0 { // the default floor is the caller's: these tests are the caller
		o.MinConfidence = DefaultCarveMinConfidence
	}
	if o.Partition == 0 {
		o.Partition = -1
	}
	cs, err := e.s.Carve(context.Background(), o)
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	return cs
}

func carveAuditEntries(t *testing.T, c *evidence.Case, action string) []evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range es {
		if action == "" || e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// carveWarnCodes returns the code of every analysis.warning of the audit log.
func carveWarnCodes(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	var out []string
	for _, e := range carveAuditEntries(t, c, "analysis.warning") {
		code, _ := e.Details["code"].(string)
		out = append(out, code)
	}
	return out
}

func carvedRecords(t *testing.T, c *evidence.Case) []evidence.ManifestRecord {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == evidence.KindCarve {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b evidence.ManifestRecord) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func carveArtifactBytes(t *testing.T, c *evidence.Case, r evidence.ManifestRecord) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(r.Path)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func carveVerify(t *testing.T, e *carveEnv) evidence.VerifyReport {
	t.Helper()
	opts, _ := carveOpts(nil)
	rep, err := e.c.VerifyWith(context.Background(), VerifyChecks(opts)...)
	if err != nil {
		t.Fatalf("VerifyWith: %v", err)
	}
	return rep
}

func TestCarveWritesValidatedObjects(t *testing.T) {
	jpg, png := carveJPEG(), carvePNG()
	nodes := carveStdNodes(jpg, png)
	e := newCarveEnv(t, nodes...)
	jOff, pOff := carveImageOff(t, "/del1.jpg", nodes...), carveImageOff(t, "/del2.png", nodes...)
	cs := e.carve(t, CarveOptions{})
	if cs.Carved != 2 || cs.Partial != 0 || cs.Files != 2 {
		t.Fatalf("summary %+v", cs)
	}
	recs := carvedRecords(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d carved artifacts, want 2", len(recs))
	}
	for i, tc := range []struct {
		suffix string
		obj    []byte
		off    int64
		sig    string
	}{
		{fmt.Sprintf("carved/p1-mtfs/%016x-jpeg.jpg", jOff), jpg, jOff, "jpeg"},
		{fmt.Sprintf("carved/p1-mtfs/%016x-png.png", pOff), png, pOff, "png"},
	} {
		r := recs[i]
		if !strings.HasSuffix(r.Path, tc.suffix) {
			t.Errorf("path %q, want suffix %q", r.Path, tc.suffix)
		}
		if got := carveArtifactBytes(t, e.c, r); !bytes.Equal(got, tc.obj) {
			t.Errorf("%s: %d bytes differ from the %d-byte object", r.Path, len(got), len(tc.obj))
		}
		d := r.Source.Derived
		rv := d.Recovery
		if d.ParentID != e.rec.ID || d.Partition != 1 || d.FSType != "mtfs" || d.PartitionOffset != int64(carveFirstLBA)*512 ||
			len(d.Runs) != 1 || d.Runs[0] != (evidence.Run{Offset: tc.off, Length: int64(len(tc.obj))}) || d.AcquisitionGaps != nil {
			t.Errorf("%s derivation %+v", r.Path, d)
		}
		if rv.Class != evidence.ClassCarved || rv.Method != "carve-validated" || rv.Confidence == nil || *rv.Confidence != 70 ||
			rv.Alloc != (evidence.AllocSummary{Free: int64(len(tc.obj))}) || rv.Scope != CarveUnallocated || rv.Content != "type-match" ||
			rv.Algorithm != evidence.AlgorithmRecover {
			t.Errorf("%s recovery %+v", r.Path, rv)
		}
		if len(rv.Basis) < 3 || rv.Basis[0] != "sig="+tc.sig || rv.Basis[1] != "validator="+tc.sig || !strings.HasPrefix(rv.Basis[len(rv.Basis)-1], "scope-run=") {
			t.Errorf("%s basis %q", r.Path, rv.Basis)
		}
		if len(rv.Assumptions) == 0 || rv.Assumptions[0] != "contiguous-object" {
			t.Errorf("%s assumptions %q", r.Path, rv.Assumptions)
		}
		want := map[string]string{
			"sigset": carve.BuiltinID, "sigset_sha256": carve.Builtin().Digest(), "min_confidence": "40",
			"nested": "false", "unaligned": "false", "align": strconv.Itoa(carveBS), "types": "all",
		}
		if len(rv.Params) != len(want) {
			t.Errorf("%s params %v, want %v", r.Path, rv.Params, want)
		}
		for k, v := range want {
			if rv.Params[k] != v {
				t.Errorf("%s param %s = %q, want %q", r.Path, k, rv.Params[k], v)
			}
		}
	}
	rep := carveVerify(t, e)
	if !rep.OK() || rep.RecoveredReproduced != 2 {
		t.Errorf("verify: problems %v, reproduced %d", rep.Problems, rep.RecoveredReproduced)
	}
}

func TestCarveNeverModifiesSource(t *testing.T) {
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	e := newCarveEnv(t, nodes...)
	file := filepath.Join(e.c.Dir, filepath.FromSlash(e.rec.Path))
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	line := func() string {
		recs, err := e.c.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.ID == e.rec.ID {
				return fmt.Sprintf("%+v", r)
			}
		}
		return ""
	}
	l0 := line()
	e.carve(t, CarveOptions{})
	e.carve(t, CarveOptions{Scope: CarveRaw})
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || line() != l0 || l0 == "" {
		t.Error("the parent image or its manifest line changed")
	}
}

// MinConfidence 0 is a floor of 0 (the default of 40 belongs to the caller, as for recover): a hit capped to
// confidence 0 by its chain is written at 0 and skipped at 1.
func TestCarveFloorZeroWritesConfidenceZero(t *testing.T) {
	zero := 0
	s, c, _ := carveArtifactSession(t, &zero, nil, false)
	pl, err := s.PlanCarve(context.Background(), CarveOptions{Partition: -1, MinConfidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Hits) != 1 || pl.Hits[0].Confidence != 0 || pl.Hits[0].Skip != carveSkipBelowMin {
		t.Fatalf("floor 1: %+v", pl.Hits)
	}
	cs, err := s.Carve(context.Background(), CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	recs := carvedRecords(t, c)
	if cs.Carved != 1 || len(recs) != 1 || *recs[0].Source.Derived.Recovery.Confidence != 0 || recs[0].Source.Derived.Recovery.Params["min_confidence"] != "0" {
		t.Fatalf("floor 0: carved %d, %+v", cs.Carved, recs)
	}
}

// The default floor is the caller's: a bounded hit (25) is listed but not written at DefaultCarveMinConfidence.
func TestCarveDefaultMinConfidenceIsTheCallers(t *testing.T) {
	nodes := []fstest.Node{carveDeleted("/c.bin", append([]byte{0xca, 0xfe, 0xba, 0xbe}, carveFill(9, 600)...))}
	e := newCarveEnv(t, nodes...)
	for floor, want := range map[int]string{DefaultCarveMinConfidence: carveSkipBelowMin, 0: ""} {
		pl, err := e.s.PlanCarve(context.Background(), CarveOptions{Partition: -1, SigFile: sigCafe(t), Types: []string{"cafe"}, MinConfidence: floor})
		if err != nil {
			t.Fatal(err)
		}
		if len(pl.Hits) != 1 || pl.Hits[0].Skip != want || pl.Hits[0].Confidence != 25 {
			t.Fatalf("floor %d: hits %+v", floor, pl.Hits)
		}
	}
}

func sigCafe(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"format":"` + carve.SigFileFormat + `","types":[{"name":"cafe","ext":"cfe","mime":"application/x-cafe",` +
		`"headers":[{"offset":0,"hex":"cafebabe","mask":""}],"footer":"","min_len":8,"max_len":512,"validate":"","align":"block"}]}`)
}

func TestCarveMinConfidenceFloor(t *testing.T) {
	nodes := []fstest.Node{carveDeleted("/c.bin", append([]byte{0xca, 0xfe, 0xba, 0xbe}, carveFill(9, 600)...))}
	e := newCarveEnv(t, nodes...)
	off := carveImageOff(t, "/c.bin", nodes...)
	cs := e.carve(t, CarveOptions{SigFile: sigCafe(t), Types: []string{"cafe"}}) // the default floor 40
	if cs.Carved != 0 || cs.SkippedBy[carveSkipBelowMin] != 1 || len(carvedRecords(t, e.c)) != 0 {
		t.Fatalf("summary %+v skipped %v", cs, cs.SkippedBy)
	}
	var found bool
	for _, w := range carveAuditEntries(t, e.c, "analysis.warning") {
		if w.Details["code"] == carveSkipBelowMin && strings.Contains(fmt.Sprint(w.Details["path"]), fmt.Sprintf("%016x", off)) {
			found = true
		}
	}
	if !found {
		t.Error("no analysis.warning names the offset and the reason")
	}
	cs = e.carve(t, CarveOptions{SigFile: sigCafe(t), Types: []string{"cafe"}, MinConfidence: 20})
	recs := carvedRecords(t, e.c)
	if cs.Carved != 1 || len(recs) != 1 {
		t.Fatalf("at 20: summary %+v, %d records", cs, len(recs))
	}
	rv := recs[0].Source.Derived.Recovery
	if rv.Method != "carve-bounded" || *rv.Confidence != 25 || rv.Content != "ok" || rv.Params["sig_file_sha256"] == "" || recs[0].Size != 512 {
		t.Errorf("recovery %+v size %d", rv, recs[0].Size)
	}
	if !slices.Contains(rv.Assumptions, "length-capped") || rv.Basis[1] != "validator=none" {
		t.Errorf("assumptions %q basis %q", rv.Assumptions, rv.Basis)
	}
}

func TestCarveListMatchesRun(t *testing.T) {
	nodes := append(carveStdNodes(carveJPEG(), carvePNG()),
		carveDeleted("/c.bin", append([]byte{0xca, 0xfe, 0xba, 0xbe}, carveFill(9, 600)...)))
	e := newCarveEnv(t, nodes...)
	o := CarveOptions{Partition: -1, SigFile: sigCafe(t), MinConfidence: DefaultCarveMinConfidence}
	before := len(carveAuditEntries(t, e.c, ""))
	pl, err := e.s.PlanCarve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(carveAuditEntries(t, e.c, "")); n != before {
		t.Fatalf("PlanCarve wrote %d audit entries", n-before)
	}
	if len(carvedRecords(t, e.c)) != 0 {
		t.Fatal("PlanCarve wrote an artifact")
	}
	cs, err := e.s.Carve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var wantNames []string
	skipped := map[string]int{}
	for _, h := range pl.Hits {
		if h.Skip == "" {
			wantNames = append(wantNames, h.Name)
			if h.Alloc.Free != h.Length || h.Method == "" || h.Confidence == 0 {
				t.Errorf("plan hit %+v", h)
			}
		} else {
			skipped[h.Skip]++
		}
	}
	var got []string
	recs := carvedRecords(t, e.c)
	for _, r := range recs {
		got = append(got, r.Path)
	}
	if len(wantNames) != 2 || len(got) != 2 {
		t.Fatalf("plan names %v, run paths %v", wantNames, got)
	}
	for i := range got {
		if !strings.HasSuffix(got[i], wantNames[i]) {
			t.Errorf("run wrote %q, plan said %q", got[i], wantNames[i])
		}
	}
	if skipped[carveSkipBelowMin] != 1 || cs.SkippedBy[carveSkipBelowMin] != 1 || pl.SkippedBy[carveSkipBelowMin] != 1 {
		t.Errorf("skips: plan hits %v, plan %v, run %v", skipped, pl.SkippedBy, cs.SkippedBy)
	}
	if pl.Stats.Scanned != cs.Stats.Scanned || pl.ScopeBytes == 0 || pl.Sigset != cs.Sigset || pl.SigsetSHA256 != cs.SigsetSHA256 {
		t.Errorf("plan %+v vs run %+v", pl, cs)
	}
}

func TestCarveOutputBudget(t *testing.T) {
	jpg, png := carveJPEG(), carvePNG()
	nodes := append(carveStdNodes(jpg, png), carveDeleted("/del3.png", png))
	t.Run("max-files", func(t *testing.T) {
		e := newCarveEnv(t, nodes...)
		cs := e.carve(t, CarveOptions{MaxFiles: 1})
		if cs.Carved != 1 || cs.LimitReached != "max-files" || cs.NotScannedBytes <= 0 || cs.SkippedBy["max-files"] != 1 {
			t.Fatalf("summary carved %d limit %q not-scanned %d skipped %v", cs.Carved, cs.LimitReached, cs.NotScannedBytes, cs.SkippedBy)
		}
		if n := countCode(t, e.c, "limit"); n != 1 {
			t.Errorf("%d limit warnings, want 1", n)
		}
		end := carveAuditEntries(t, e.c, "analysis.end")
		if len(end) != 1 || end[0].Details["limit_reached"] != "max-files" {
			t.Errorf("analysis.end %+v", end)
		}
	})
	t.Run("max-bytes", func(t *testing.T) {
		e := newCarveEnv(t, nodes...)
		cs := e.carve(t, CarveOptions{MaxBytes: int64(len(jpg))})
		if cs.Carved != 1 || cs.LimitReached != "max-bytes" || cs.NotScannedBytes <= 0 || cs.SkippedBy["max-bytes"] != 1 {
			t.Fatalf("summary carved %d limit %q skipped %v", cs.Carved, cs.LimitReached, cs.SkippedBy)
		}
		if recs := carvedRecords(t, e.c); len(recs) != 1 || recs[0].Size != int64(len(jpg)) {
			t.Errorf("records %+v", recs)
		}
	})
	t.Run("plan", func(t *testing.T) {
		e := newCarveEnv(t, nodes...)
		pl, err := e.s.PlanCarve(context.Background(), CarveOptions{Partition: -1, MaxFiles: 1})
		if err != nil {
			t.Fatal(err)
		}
		if pl.LimitReached != "max-files" || len(pl.Hits) != 2 || pl.Hits[0].Skip != "" || pl.Hits[1].Skip != "max-files" {
			t.Errorf("plan limit %q hits %+v", pl.LimitReached, pl.Hits)
		}
	})
}

func countCode(t *testing.T, c *evidence.Case, code string) int {
	t.Helper()
	var n int
	for _, w := range carveWarnCodes(t, c) {
		if w == code {
			n++
		}
	}
	return n
}

func TestCarveCaseWriteFailureAborts(t *testing.T) {
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	failure := errors.New("create refused")
	e := newCarveEnv(t, nodes...)
	e.s.SetNewArtifact(func(string, string, string, evidence.Source) (*evidence.ArtifactWriter, error) { return nil, failure })
	_, err := e.s.Carve(context.Background(), CarveOptions{Partition: -1})
	var cw *caseWriteError
	if !errors.Is(err, failure) || !errors.As(err, &cw) {
		t.Fatalf("err = %v, want the case failure as a caseWriteError", err)
	}
	if len(carveAuditEntries(t, e.c, "analysis.error")) != 1 {
		t.Error("want analysis.error")
	}
	if len(carveAuditEntries(t, e.c, "analysis.warning")) != 0 {
		t.Error("an analysis.warning stands in for the failure")
	}
}

func TestCarveTwiceNeverOverwrites(t *testing.T) {
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	e := newCarveEnv(t, nodes...)
	a := e.carve(t, CarveOptions{})
	b := e.carve(t, CarveOptions{})
	if a.AnalysisID == b.AnalysisID {
		t.Fatal("equal analysis ids")
	}
	recs := carvedRecords(t, e.c)
	if len(recs) != 4 {
		t.Fatalf("%d artifacts, want 4", len(recs))
	}
	dirs := map[string][]string{}
	for _, r := range recs {
		dirs[filepath.Dir(r.Path)] = append(dirs[filepath.Dir(r.Path)], r.SHA256)
	}
	if len(dirs) != 2 {
		t.Fatalf("%d analysis directories: %v", len(dirs), dirs)
	}
	var sets [][]string
	for _, s := range dirs {
		slices.Sort(s)
		sets = append(sets, s)
	}
	if !slices.Equal(sets[0], sets[1]) {
		t.Errorf("SHA-256 sets differ: %v", sets)
	}
	if rep := carveVerify(t, e); !rep.OK() {
		t.Errorf("verify: %v", rep.Problems)
	}
}

func TestCarveNestedOption(t *testing.T) {
	inner := carvePNG()
	outer := carvetest.JPEG(carvetest.JPEGOpts{Thumbnail: inner})
	nodes := []fstest.Node{carveDeleted("/o.jpg", outer)}
	e := newCarveEnv(t, nodes...)
	off := carveImageOff(t, "/o.jpg", nodes...)
	cs := e.carve(t, CarveOptions{})
	if cs.Carved != 1 {
		t.Fatalf("default: %d artifacts, want 1", cs.Carved)
	}
	e2 := newCarveEnv(t, nodes...)
	cs = e2.carve(t, CarveOptions{Nested: true})
	recs := carvedRecords(t, e2.c)
	if cs.Carved != 2 || len(recs) != 2 {
		t.Fatalf("nested: %d artifacts, want 2", cs.Carved)
	}
	var found bool
	for _, r := range recs {
		rv := r.Source.Derived.Recovery
		if slices.Contains(rv.Assumptions, fmt.Sprintf("nested-in=%016x", off)) {
			found = true
			if rv.Params["nested"] != "true" || !bytes.Equal(carveArtifactBytes(t, e2.c, r), inner) {
				t.Errorf("nested artifact %+v", rv)
			}
		}
	}
	if !found {
		t.Error("no artifact carries nested-in")
	}
}

func TestCarveRawScopeAlloc(t *testing.T) {
	jpg, png := carveJPEG(), carvePNG()
	nodes := []fstest.Node{
		{Path: "/live.jpg", Data: carvePad(jpg)},
		carveDeleted("/del.png", png),
	}
	e := newCarveEnv(t, nodes...)
	cs := e.carve(t, CarveOptions{Scope: CarveRaw})
	recs := carvedRecords(t, e.c)
	if cs.Carved != 2 || len(recs) != 2 {
		t.Fatalf("carved %d", cs.Carved)
	}
	for _, r := range recs {
		d, rv := r.Source.Derived, r.Source.Derived.Recovery
		if rv.Class != evidence.ClassCarved || rv.Scope != CarveRaw || !strings.Contains(r.Path, "carved/raw/") {
			t.Errorf("%s: %+v", r.Path, rv)
		}
		if d.Partition != 1 || d.FSType != "mtfs" || d.PartitionOffset != int64(carveFirstLBA)*512 {
			t.Errorf("%s: region %d at %d (%q)", r.Path, d.Partition, d.PartitionOffset, d.FSType)
		}
		switch {
		case strings.HasSuffix(r.Path, ".jpg") && rv.Alloc != (evidence.AllocSummary{Allocated: r.Size}):
			t.Errorf("live object alloc %+v", rv.Alloc)
		case strings.HasSuffix(r.Path, ".png") && rv.Alloc != (evidence.AllocSummary{Free: r.Size}):
			t.Errorf("deleted object alloc %+v", rv.Alloc)
		}
	}
}

func TestCarveAnalysisAudit(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	e.carve(t, CarveOptions{})
	start := carveAuditEntries(t, e.c, "analysis.start")
	if len(start) != 1 {
		t.Fatalf("%d analysis.start", len(start))
	}
	var keys []string
	for k := range start[0].Details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{
		"algorithm", "analysis_id", "artifact_id", "export_id", "max_bytes", "max_file_size", "max_files", "min_confidence", "nested",
		"op", "parent_id", "partition", "scope", "sig_file_bytes", "sig_file_sha256", "sigset", "sigset_sha256", "types", "unaligned",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("analysis.start keys %v, want %v", keys, want)
	}
	if start[0].Details["op"] != "carve" || start[0].Details["scope"] != CarveUnallocated || start[0].Details["parent_id"] != e.rec.ID {
		t.Errorf("start %v", start[0].Details)
	}
	end := carveAuditEntries(t, e.c, "analysis.end")
	if len(end) != 1 {
		t.Fatalf("%d analysis.end", len(end))
	}
	for _, k := range carveEndKeys {
		if _, ok := end[0].Details[k]; !ok {
			t.Errorf("analysis.end lacks %q", k)
		}
	}
	if fmt.Sprint(end[0].Details["carved"]) != "2" {
		t.Errorf("carved = %v", end[0].Details["carved"])
	}
}

// carveEndKeys are the statistics of analysis.end / analysis.error (3E-R11).
var carveEndKeys = []string{
	"scope_runs", "scope_bytes", "scanned_bytes", "not_scanned_bytes", "align", "hits_by_outcome", "rejected_by", "carved",
	"partial", "skipped_by", "nested_depth_skipped", "types_capped", "unreadable_bytes", "uncopied_bytes", "validate_bytes", "limit_reached",
}

// carveArtifactSession adds a PNG-holding image and a recovered artifact over it to a new case; mutate shapes
// the recovery description and cutParent breaks the chain. The session reads the artifact raw.
func carveArtifactSession(t *testing.T, conf *int, mutate func(*evidence.Recovery), cutParent bool) (*Session, *evidence.Case, evidence.ManifestRecord) {
	t.Helper()
	c := evidencetest.NewCase(t)
	data := carvePad(carvePNG())
	parent := evidencetest.AddImage(t, c, data)
	spec := evidencetest.RecoveredSpec{Confidence: conf, Runs: []evidence.Run{{Offset: 0, Length: int64(len(data))}}, Mutate: mutate}
	rec := evidencetest.AddRecovered(t, c, parent, data, spec)
	if cutParent {
		evidencetest.RewriteManifest(t, c.Dir, rec.ID, func(r *evidence.ManifestRecord) { r.Source.Derived.ParentID = "no-such-parent" })
	}
	opts, _ := carveOpts(nil)
	s, err := OpenArtifactScope(c, rec.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c, rec
}

func TestCarveArtifactCapByChain(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		name    string
		conf    *int
		mutate  func(*evidence.Recovery)
		cut     bool
		want    int
		assumes []string
		clean   bool
	}{
		{name: "intact chain at 30", conf: ptr(30), want: 30, assumes: []string{"parent-recovered="}, clean: true},
		{name: "cut chain at 60", conf: ptr(60), cut: true, want: 25, assumes: []string{"parent-recovered=", "recovery-cap-unknown"}},
		{name: "cut chain at 10 stays 10", conf: ptr(10), cut: true, want: 10, assumes: []string{"recovery-cap-unknown"}},
		{name: "no confidence", mutate: func(r *evidence.Recovery) { r.Confidence = nil }, want: 25, assumes: []string{"recovery-cap-unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, art := carveArtifactSession(t, tc.conf, tc.mutate, tc.cut)
			cs, err := s.Carve(context.Background(), CarveOptions{Partition: -1, MinConfidence: 1})
			if err != nil {
				t.Fatal(err)
			}
			recs := carvedRecords(t, c)
			if cs.Carved != 1 || len(recs) != 1 {
				t.Fatalf("carved %d", cs.Carved)
			}
			r := recs[0]
			d, rv := r.Source.Derived, r.Source.Derived.Recovery
			if *rv.Confidence != tc.want {
				t.Errorf("confidence %d, want %d", *rv.Confidence, tc.want)
			}
			for _, a := range tc.assumes {
				if !slices.ContainsFunc(rv.Assumptions, func(x string) bool { return strings.HasPrefix(x, a) }) {
					t.Errorf("assumptions %q lack %q", rv.Assumptions, a)
				}
			}
			if rv.Scope != CarveArtifact || !strings.Contains(r.Path, "carved/raw/") || rv.Alloc != (evidence.AllocSummary{Unknown: r.Size}) ||
				d.AcquisitionGaps != nil || d.ParentID != art.ID || d.ParentSegments != nil || d.Partition != 0 || d.FSType != "" {
				t.Errorf("scope %q path %q alloc %+v derivation %+v", rv.Scope, r.Path, rv.Alloc, d)
			}
			if tc.clean {
				if !slices.Contains(rv.Assumptions, "parent-recovered="+art.ID) {
					t.Errorf("assumptions %q", rv.Assumptions)
				}
				if rep, err := c.VerifyWith(context.Background()); err != nil || !rep.OK() {
					t.Errorf("verify: %v %v", rep.Problems, err)
				}
			}
		})
	}
}

// TestCarveTrailCapMatchesDBRecoverTwin pins carveTrailCap to the rule documented on dbrecover.trailCap: a
// chain with no recovered artifact is not capped; a recovered chain is capped at its lowest confidence; one
// with no confidence, or cut by a missing parent, is unknown; a cut chain above 25 is capped at 25, one at or
// below keeps its minimum. The rule is written out here; dbrecover is not imported.
func TestCarveTrailCapMatchesDBRecoverTwin(t *testing.T) {
	img := evidence.ManifestRecord{ID: "img"}
	rec := func(id, parent string, conf *int) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: id, Source: evidence.Source{Kind: evidence.KindRecover, Derived: &evidence.Derivation{
			ParentID: parent, Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Confidence: conf},
		}}}
	}
	ptr := func(n int) *int { return &n }
	live := evidence.ManifestRecord{ID: "live", Source: evidence.Source{Kind: "extract", Derived: &evidence.Derivation{ParentID: "img"}}}
	for _, tc := range []struct {
		name      string
		start     string
		recs      []evidence.ManifestRecord
		recovered bool
		ceiling   *int
		unknown   bool
	}{
		{"not recovered", "live", []evidence.ManifestRecord{img, live}, false, nil, false},
		{"one recovered at 30", "a", []evidence.ManifestRecord{img, rec("a", "img", ptr(30))}, true, ptr(30), false},
		{"minimum of two", "a", []evidence.ManifestRecord{img, rec("b", "img", ptr(60)), rec("a", "b", ptr(40))}, true, ptr(40), false},
		{"no confidence", "a", []evidence.ManifestRecord{img, rec("a", "img", nil)}, true, nil, true},
		{"cut at 60", "a", []evidence.ManifestRecord{rec("a", "gone", ptr(60))}, true, ptr(25), true},
		{"cut at 25", "a", []evidence.ManifestRecord{rec("a", "gone", ptr(25))}, true, ptr(25), true},
		{"cut at 10", "a", []evidence.ManifestRecord{rec("a", "gone", ptr(10))}, true, ptr(10), true},
		{"cut, no confidence", "a", []evidence.ManifestRecord{rec("a", "gone", nil)}, true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byID := map[string]evidence.ManifestRecord{}
			for _, r := range tc.recs {
				byID[r.ID] = r
			}
			recovered, ceiling, unknown := carveTrailCap(byID, tc.start)
			if recovered != tc.recovered || unknown != tc.unknown || (ceiling == nil) != (tc.ceiling == nil) || (ceiling != nil && *ceiling != *tc.ceiling) {
				t.Errorf("got (%v, %v, %v), want (%v, %v, %v)", recovered, ceiling, unknown, tc.recovered, tc.ceiling, tc.unknown)
			}
		})
	}
}

func TestCarveFromUnallocBinUsesRunBoundaries(t *testing.T) {
	png := carvePNG()
	nodes := []fstest.Node{
		{Path: "/a.bin", Data: carveFill(1, carveBS)},
		{Path: "/f1.bin", Data: carveFill(2, 4*carveBS), Fragments: 2}, // a free block between its runs
		{Path: "/f2.bin", Data: carveFill(3, 4*carveBS), Fragments: 2},
	}
	base := newCarveEnv(t, nodes...)
	fsys, part, err := base.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	runs, _, err := base.s.unallocatedRunsNote(fsys, part, false)
	if err != nil || len(runs) < 3 {
		t.Fatalf("free runs %v, %v", runs, err)
	}
	r1, r2, last := runs[0], runs[1], runs[len(runs)-1]
	if r1.Offset+r1.Length == r2.Offset {
		t.Fatal("test setup: adjacent runs")
	}
	img := slices.Clone(base.img)
	// an object across the joint of two runs that are adjacent in the export, and one wholly inside the last run
	const head = 20
	copy(img[r1.Offset+r1.Length-head:], png[:head])
	copy(img[r2.Offset:], png[head:])
	whole := last.Offset + carveBS
	copy(img[whole:], png)

	e := newCarveImageEnv(t, img, nil)
	sum, err := e.s.ExportUnallocated(context.Background(), UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	var bin evidence.ManifestRecord
	for _, r := range sum.Artifacts {
		if r.Source.Kind == "unallocated" {
			bin = r
		}
	}
	if raw := carveArtifactBytes(t, e.c, bin); bytes.Count(raw, png[:8]) != 2 || !bytes.Contains(raw, png) {
		t.Fatal("test setup: the export must hold the joint object contiguously and the whole object")
	}
	opts, _ := carveOpts(nil)
	s, err := OpenArtifactScope(e.c, bin.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cs, err := s.Carve(context.Background(), CarveOptions{Partition: -1, Unaligned: true})
	if err != nil {
		t.Fatal(err)
	}
	recs := carvedRecords(t, e.c)
	if cs.Carved != 1 || len(recs) != 1 {
		t.Fatalf("carved %d, want only the object inside one run", cs.Carved)
	}
	d, rv := recs[0].Source.Derived, recs[0].Source.Derived.Recovery
	if d.ParentID != e.rec.ID || len(d.Runs) != 1 || d.Runs[0] != (evidence.Run{Offset: whole, Length: int64(len(png))}) || rv.Scope != CarveUnallocated {
		t.Errorf("derivation %+v scope %q", d, rv.Scope)
	}
	start := carveAuditEntries(t, e.c, "analysis.start")
	if last := start[len(start)-1].Details; last["export_id"] != bin.ID || last["op"] != "carve" {
		t.Errorf("start %v", last)
	}
}
