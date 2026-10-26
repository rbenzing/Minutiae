package examine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// ReportsSlack lets the lying wrapper of the planner tests pass the capability check of ExportSlack: the
// wrapper changes answers, not what the reader can state.
func (f *slackFakeFS) ReportsSlack() bool { return filesys.ReportsSlack(f.FileSystem) }

// ---- helpers -------------------------------------------------------------------------------------------

func exportSlack(t *testing.T, s *Session, o SlackOptions) (SlackSummary, error) {
	t.Helper()
	return s.ExportSlack(context.Background(), o)
}

func mustExportSlack(t *testing.T, s *Session, o SlackOptions) SlackSummary {
	t.Helper()
	sum, err := exportSlack(t, s, o)
	if err != nil {
		t.Fatalf("ExportSlack: %v", err)
	}
	return sum
}

func slackArtBytes(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) []byte {
	t.Helper()
	f, _, err := c.OpenArtifact(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func slackLinesOf(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord, method string) []evidence.SlackMapLine {
	t.Helper()
	var out []evidence.SlackMapLine
	err := evidence.ReadSlackRunMap(bytes.NewReader(slackArtBytes(t, c, rec)), method, func(l evidence.SlackMapLine) error {
		out = append(out, l)
		return nil
	})
	if err != nil {
		t.Fatalf("read the run map %s: %v", rec.Path, err)
	}
	return out
}

func slackRecord(t *testing.T, sum SlackSummary, suffix string) evidence.ManifestRecord {
	t.Helper()
	for _, r := range sum.Artifacts {
		if strings.HasSuffix(r.Path, suffix) {
			return r
		}
	}
	t.Fatalf("no artifact ending in %q among %d", suffix, len(sum.Artifacts))
	return evidence.ManifestRecord{}
}

func verifyClean(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("case verify found problems: %v", rep.Problems)
	}
	return rep
}

func auditSince(t *testing.T, c *evidence.Case, n0 int) []evidence.AuditEntry {
	t.Helper()
	es, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	return es[n0:]
}

func auditLen(t *testing.T, c *evidence.Case) int {
	t.Helper()
	es, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func segBytes(img []byte, segs []SlackSegment) []byte {
	var out []byte
	for _, s := range segs {
		out = append(out, img[s.ImageOffset:s.ImageOffset+s.Length]...)
	}
	return out
}

// fullFAT is threeTails with a non-uniform volume tail: three file segments and one volume segment.
func fullFAT(t *testing.T) (*slackFAT, []byte) {
	t.Helper()
	f, _ := threeTails(t)
	return f, slices.Concat(f.img, pat(8192, 3))
}

// ---- tests ---------------------------------------------------------------------------------------------

func TestExportSlackWritesArtifactsAndSidecars(t *testing.T) {
	f, img := fullFAT(t)
	s := slackSession(t, img, Options{})
	plan := slackPlanOf(t, s, SlackOptions{Partition: -1})
	if len(plan.file) != 3 || len(plan.vol) != 1 {
		t.Fatalf("setup: plan has %d file and %d volume segments", len(plan.file), len(plan.vol))
	}
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1})

	want := []struct{ suffix, kind string }{
		{"/p0-fat16/slack.runs.jsonl", "runs"},
		{"/p0-fat16/slack.bin", evidence.KindSlack},
		{"/p0-fat16/volume-slack.runs.jsonl", "runs"},
		{"/p0-fat16/volume-slack.bin", evidence.KindSlack},
	}
	if len(sum.Artifacts) != len(want) {
		t.Fatalf("%d artifacts, want %d: %+v", len(sum.Artifacts), len(want), sum.Artifacts)
	}
	for i, w := range want {
		a := sum.Artifacts[i]
		if !strings.HasSuffix(a.Path, w.suffix) || a.Source.Kind != w.kind || !strings.Contains(a.Path, "/slack/") {
			t.Errorf("artifact %d = %q kind %q, want %q kind %q", i, a.Path, a.Source.Kind, w.suffix, w.kind)
		}
	}
	fileBin, volBin := sum.Artifacts[1], sum.Artifacts[3]
	var fileTotal int64
	for _, g := range plan.file {
		fileTotal += g.Length
	}
	rv := fileBin.Source.Derived.Recovery
	if rv.Class != evidence.ClassSlack || rv.Method != evidence.MethodSlackFile || rv.Confidence == nil || *rv.Confidence != 10 ||
		rv.Algorithm != AlgorithmSlack || rv.Content != "ok" || !slices.Equal(rv.Basis, []string{"allocated-units-of-live-files"}) ||
		rv.Alloc != (evidence.AllocSummary{Allocated: fileTotal}) {
		t.Errorf("file recovery = %+v", rv)
	}
	if rv.Params["uniform_segments"] != "0" || rv.Params["unit"] != fmt.Sprint(f.cs()) || rv.Params["include_zero"] != "false" || rv.Params["segments"] != "3" {
		t.Errorf("file params = %v", rv.Params)
	}
	vr := volBin.Source.Derived.Recovery
	wantBasis := []string{fmt.Sprintf("filesystem-size=%d", plan.fsSize), fmt.Sprintf("partition-length=%d", plan.part.Length)}
	if vr.Method != evidence.MethodSlackVolume || *vr.Confidence != 10 || vr.Alloc != (evidence.AllocSummary{Unknown: plan.vol[0].Length}) ||
		!slices.Equal(vr.Basis, wantBasis) || vr.Params["chunk"] != "4096" {
		t.Errorf("volume recovery = %+v", vr)
	}
	if fileBin.Incomplete || volBin.Incomplete || sum.Files != 2 || sum.Incomplete {
		t.Errorf("complete run reported incomplete: %+v", sum)
	}
	if !bytes.Equal(slackArtBytes(t, s.Case, fileBin), segBytes(img, plan.file)) {
		t.Error("slack.bin is not the concatenation of the planned segments")
	}
	if !bytes.Equal(slackArtBytes(t, s.Case, volBin), segBytes(img, plan.vol)) {
		t.Error("volume-slack.bin is not the planned segment")
	}
	lines := slackLinesOf(t, s.Case, sum.Artifacts[0], evidence.MethodSlackFile)
	if len(lines) != len(plan.file) {
		t.Fatalf("%d map lines, want %d", len(lines), len(plan.file))
	}
	var off int64
	for i, l := range lines {
		g := plan.file[i]
		w := evidence.SlackMapLine{Offset: off, Length: g.Length, ImageOffset: g.ImageOffset, FSPath: g.FSPath, FSID: g.FSID, FileSize: g.FileSize, Uniform: g.Uniform, Exported: true}
		if l != w {
			t.Errorf("line %d = %+v, want %+v", i, l, w)
		}
		off += g.Length
	}
	if vl := slackLinesOf(t, s.Case, sum.Artifacts[2], evidence.MethodSlackVolume); len(vl) != 1 || vl[0].ImageOffset != plan.vol[0].ImageOffset || !vl[0].Exported {
		t.Errorf("volume map = %+v", vl)
	}
	if sum.FileSegments != 3 || sum.VolumeSegments != 1 || sum.FileBytes != fileTotal || sum.VolumeBytes != plan.vol[0].Length ||
		sum.FilesExamined != 3 || sum.FSType != "fat16" || sum.Unit != f.cs() || sum.Bytes != fileTotal+plan.vol[0].Length {
		t.Errorf("summary = %+v", sum)
	}
	verifyClean(t, s.Case)
}

func TestExportSlackAuditBeforeWrite(t *testing.T) {
	_, img := fullFAT(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{})
	n0 := auditLen(t, c)
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1})
	es := auditSince(t, c, n0)
	if len(es) < 6 || es[0].Action != "analysis.start" || es[len(es)-1].Action != "analysis.end" {
		t.Fatalf("audit entries: %v", actions(es))
	}
	st := es[0].Details
	if st["op"] != "slack" || st["format"] != AlgorithmSlack || st["fs_type"] != "fat16" || st["files"] != true || st["volume"] != true ||
		st["include_zero"] != false || fmt.Sprint(st["partition"]) != "0" || st["parent_id"] != s.Parent.ID {
		t.Errorf("analysis.start details = %v", st)
	}
	if fmt.Sprint(st["max_bytes"]) != fmt.Sprint(DefaultMaxBytes) {
		t.Errorf("max_bytes = %v", st["max_bytes"])
	}
	creates, lastWarn := 0, -1
	firstCreate := -1
	for i, e := range es {
		switch e.Action {
		case "artifact.create":
			creates++
			if firstCreate < 0 {
				firstCreate = i
			}
		case "analysis.warning":
			lastWarn = i
		}
	}
	if creates != 4 || (lastWarn >= 0 && lastWarn > firstCreate) {
		t.Errorf("creates %d, last warning at %d, first create at %d: %v", creates, lastWarn, firstCreate, actions(es))
	}
	end := es[len(es)-1].Details
	num := func(k string) int { n, _ := strconv.Atoi(fmt.Sprint(end[k])); return n }
	if num("file_segments") != sum.FileSegments || num("volume_segments") != sum.VolumeSegments || num("files_examined") != sum.FilesExamined ||
		num("uniform_segments") != sum.UniformSegments || num("walk_errors") != sum.WalkErrors || num("files") != sum.Files ||
		num("volume_uniform_chunks") != sum.VolumeUniformChunks || end["limit_reached"] != sum.LimitReached ||
		int64(num("file_bytes")) != sum.FileBytes || int64(num("volume_bytes")) != sum.VolumeBytes || end["incomplete"] != false {
		t.Errorf("analysis.end = %v, summary %+v", end, sum)
	}
	if end["skipped_by"] == nil {
		t.Error("analysis.end has no skipped_by")
	}
}

func actions(es []evidence.AuditEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Action)
	}
	return out
}

func TestExportSlackRefusesWithoutReporter(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}})
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{Drivers: []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open}}})
	n0, m0 := auditLen(t, c), 0
	if recs, err := c.Manifest(); err == nil {
		m0 = len(recs)
	}
	_, err := exportSlack(t, s, SlackOptions{Partition: -1})
	if !errors.Is(err, filesys.ErrNoSlack) || !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "mtfs") {
		t.Fatalf("err = %v", err)
	}
	recs, _ := c.Manifest()
	if auditLen(t, c) != n0 || len(recs) != m0 {
		t.Errorf("a refused run wrote: audit %d -> %d, manifest %d -> %d", n0, auditLen(t, c), m0, len(recs))
	}
}

func TestExportSlackRejectsNegativeMaxBytes(t *testing.T) {
	_, img := fullFAT(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{})
	n0 := auditLen(t, c)
	_, err := exportSlack(t, s, SlackOptions{Partition: -1, MaxBytes: -1})
	if !errors.Is(err, ErrInvalidLimit) || auditLen(t, c) != n0 {
		t.Fatalf("err = %v, audit %d -> %d", err, n0, auditLen(t, c))
	}
}

func TestExportSlackNothingToExport(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(3)}, {Path: "B.TXT", Data: slackFill(cs + 1)}}, 2)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, f.img, Options{})
	n0 := auditLen(t, c)
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1})
	if len(sum.Artifacts) != 0 || sum.Files != 0 || sum.Incomplete {
		t.Fatalf("summary = %+v", sum)
	}
	var texts []string
	for _, w := range sum.Warnings {
		texts = append(texts, w.Reason)
	}
	all := strings.Join(texts, "|")
	if !strings.Contains(all, "no file slack exported") || !strings.Contains(all, "no volume slack exported") {
		t.Errorf("warnings = %q", all)
	}
	if sum.UniformSegments != 2 || sum.UniformBytes == 0 {
		t.Errorf("uniform %d/%d", sum.UniformSegments, sum.UniformBytes)
	}
	es := auditSince(t, c, n0)
	if len(es) == 0 {
		t.Fatal("no audit entries")
	}
	end := es[len(es)-1]
	if end.Action != "analysis.end" || fmt.Sprint(end.Details["uniform_segments"]) != "2" {
		t.Errorf("analysis.end = %+v", end)
	}
	verifyClean(t, c)
}

func TestExportSlackFilesOnlyAndVolumeOnly(t *testing.T) {
	_, img := fullFAT(t)
	s := slackSession(t, img, Options{})
	fs := mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
	if len(fs.Artifacts) != 2 || fs.Artifacts[1].Source.Derived.Recovery.Method != evidence.MethodSlackFile || fs.VolumeSegments != 0 {
		t.Fatalf("files only: %+v", fs.Artifacts)
	}
	vs := mustExportSlack(t, s, SlackOptions{Partition: -1, Volume: true})
	if len(vs.Artifacts) != 2 || vs.Artifacts[1].Source.Derived.Recovery.Method != evidence.MethodSlackVolume || vs.FileSegments != 0 || vs.FilesExamined != 0 {
		t.Fatalf("volume only: %+v", vs.Artifacts)
	}
	verifyClean(t, s.Case)
}

func TestSlackCaseWriteFailureIsNeverDowngradedToWarning(t *testing.T) {
	_, img := fullFAT(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{})
	boom := errors.New("injected disk failure")
	s.newArtifact = func(dev, acq, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
		if strings.HasSuffix(rel, "p0-fat16/slack.bin") {
			return nil, boom
		}
		return c.NewArtifact(dev, acq, rel, src)
	}
	n0 := auditLen(t, c)
	_, err := exportSlack(t, s, SlackOptions{Partition: -1})
	var cw *caseWriteError
	if !errors.As(err, &cw) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a case write error", err)
	}
	es := auditSince(t, c, n0)
	if len(es) == 0 || es[len(es)-1].Action != "analysis.error" {
		t.Errorf("last entry = %s", es[len(es)-1].Action)
	}
	for _, e := range es {
		if e.Action == "analysis.warning" && strings.Contains(fmt.Sprint(e.Details), "injected") {
			t.Errorf("the failure was downgraded to a warning: %v", e.Details)
		}
	}
}

func TestSlackReadFailureKeepsPrefix(t *testing.T) {
	f, _ := threeTails(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, f.img, Options{})
	plan := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	boom := errors.New("injected read failure")
	old := slackCopyHook
	slackCopyHook = func(i int) error {
		if i == 1 {
			return boom
		}
		return nil
	}
	t.Cleanup(func() { slackCopyHook = old })
	sum, err := exportSlack(t, s, SlackOptions{Partition: -1, Files: true})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	bin := slackRecord(t, sum, "p0-fat16/slack.bin")
	if !bin.Incomplete || bin.Size != plan.file[0].Length || !bytes.Equal(slackArtBytes(t, c, bin), segBytes(f.img, plan.file[:1])) {
		t.Fatalf("bin = %+v", bin)
	}
	d := bin.Source.Derived
	full := slackRecord(t, sum, "p0-fat16/slack.runs.jsonl")
	if !strings.HasSuffix(findByID(t, sum, d.RunsArtifact).Path, "slack.bin.incomplete.runs.jsonl") || d.Recovery.DeclaredRunsArtifact != full.ID {
		t.Errorf("runs artifact %q declared %q (full %q)", d.RunsArtifact, d.Recovery.DeclaredRunsArtifact, full.ID)
	}
	if lines := slackLinesOf(t, c, findByID(t, sum, d.RunsArtifact), evidence.MethodSlackFile); len(lines) != 1 || lines[0].Length != plan.file[0].Length {
		t.Errorf("prefix map = %+v", lines)
	}
	if got := len(slackLinesOf(t, c, full, evidence.MethodSlackFile)); got != 3 {
		t.Errorf("the full sidecar has %d lines, want 3", got)
	}
	if d.Recovery.Alloc != (evidence.AllocSummary{Allocated: bin.Size}) || !sum.Incomplete {
		t.Errorf("alloc %+v incomplete %v", d.Recovery.Alloc, sum.Incomplete)
	}
	verifyClean(t, c)
}

func findByID(t *testing.T, sum SlackSummary, id string) evidence.ManifestRecord {
	t.Helper()
	for _, r := range sum.Artifacts {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no artifact %q", id)
	return evidence.ManifestRecord{}
}

func TestSlackPrefixLinesCutsTheLastLine(t *testing.T) {
	segs := []SlackSegment{{ImageOffset: 100, Length: 10}, {ImageOffset: 300, Length: 20}, {ImageOffset: 500, Length: 30}}
	got := prefixSegments(segs, 15)
	if len(got) != 2 || got[0] != segs[0] || got[1].ImageOffset != 300 || got[1].Length != 5 {
		t.Fatalf("prefix = %+v", got)
	}
	if got := prefixSegments(segs, 10); len(got) != 1 {
		t.Errorf("exact boundary = %+v", got)
	}
	if got := prefixSegments(segs, 0); len(got) != 0 {
		t.Errorf("zero = %+v", got)
	}
	if segs[1].Length != 20 {
		t.Error("the input was changed")
	}
}

func TestSlackOutputBudget(t *testing.T) {
	cs := int64(slackTestCS)
	f, _ := threeTails(t)
	img := slices.Concat(f.img, pat(4096, 9))
	s := slackSession(t, img, Options{})
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1, MaxBytes: cs})
	if sum.LimitReached != "max-bytes" || !sum.Incomplete || sum.FileSegments != 1 || sum.SkippedBy["limit"] != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	bin := slackRecord(t, sum, "p0-fat16/slack.bin")
	if !bin.Incomplete || !strings.Contains(bin.Error, "max-bytes") || bin.Size != cs-1 {
		t.Errorf("bin = %+v", bin)
	}
	limitWarnings := 0
	for _, w := range sum.Warnings {
		if strings.HasPrefix(w.Reason, "limit: max-bytes") {
			limitWarnings++
		}
	}
	if limitWarnings != 1 {
		t.Errorf("%d limit warnings in %+v", limitWarnings, sum.Warnings)
	}
	// The volume part was not planned: its artifact is incomplete, never "no volume slack".
	vbin := slackRecord(t, sum, "p0-fat16/volume-slack.bin")
	if !vbin.Incomplete || vbin.Size != 0 {
		t.Errorf("volume bin = %+v", vbin)
	}
	for _, w := range sum.Warnings {
		if strings.Contains(w.Reason, "no volume slack exported") {
			t.Errorf("an unplanned volume was reported as having no slack: %+v", w)
		}
	}
	verifyClean(t, s.Case)
}

func TestSlackIncompleteVolumeListsOmittedRanges(t *testing.T) {
	f, _ := threeTails(t)
	img := slices.Concat(f.img, make([]byte, 4096), pat(4096, 5), pat(4096, 5))
	s := slackSession(t, img, Options{Scan: ScanBudget{Bytes: 8192}})
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1, Volume: true})
	if sum.LimitReached != "max-scan-bytes" || !sum.Incomplete || sum.VolumeSegments != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	vbin := slackRecord(t, sum, "volume-slack.bin")
	if !vbin.Incomplete || vbin.Size != 4096 {
		t.Fatalf("bin = %+v", vbin)
	}
	lines := slackLinesOf(t, s.Case, slackRecord(t, sum, "volume-slack.runs.jsonl"), evidence.MethodSlackVolume)
	if len(lines) != 2 || !lines[0].Exported || lines[1].Exported || !lines[1].Uniform || lines[1].Offset != 4096 || lines[1].Length != 4096 {
		t.Fatalf("lines = %+v", lines)
	}
	if vbin.Source.Derived.Recovery.Params["uniform_chunks"] != "1" {
		t.Errorf("params = %v", vbin.Source.Derived.Recovery.Params)
	}
	verifyClean(t, s.Case)
}

func TestSlackCutDirectoryIsIncomplete(t *testing.T) {
	f, _ := fatCutSlack(t, true)
	img := slices.Concat(f.img, pat(4096, 5))
	s := slackSession(t, img, Options{})
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1})
	if !sum.Incomplete || sum.WalkErrors != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	bin := slackRecord(t, sum, "p0-fat16/slack.bin")
	if !bin.Incomplete || !strings.Contains(bin.Error, "1 directory not read to the end") || bin.Size == 0 {
		t.Errorf("bin = %+v", bin)
	}
	if vbin := slackRecord(t, sum, "p0-fat16/volume-slack.bin"); vbin.Incomplete {
		t.Errorf("the volume artifact is complete: %+v", vbin)
	}
	if sum.Files != 1 {
		t.Errorf("Files = %d, want only the complete volume artifact", sum.Files)
	}
	verifyClean(t, s.Case)

	// Zero tails: nothing is exported, the omitted ranges are listed with exported false.
	z, _ := fatCutSlack(t, false)
	s2 := slackSession(t, z.img, Options{})
	zs := mustExportSlack(t, s2, SlackOptions{Partition: -1, Files: true})
	zbin := slackRecord(t, zs, "p0-fat16/slack.bin")
	if !zbin.Incomplete || zbin.Size != 0 || zs.UniformSegments < 2 {
		t.Fatalf("zero-tail run: bin %+v summary %+v", zbin, zs)
	}
	lines := slackLinesOf(t, s2.Case, slackRecord(t, zs, "slack.runs.jsonl"), evidence.MethodSlackFile)
	if len(lines) != zs.UniformSegments {
		t.Fatalf("%d lines for %d uniform segments", len(lines), zs.UniformSegments)
	}
	for _, l := range lines {
		if l.Exported || !l.Uniform || l.Offset != 0 {
			t.Errorf("line = %+v", l)
		}
	}
	if zbin.Source.Derived.Recovery.Params["uniform_segments"] != fmt.Sprint(zs.UniformSegments) {
		t.Errorf("params = %v", zbin.Source.Derived.Recovery.Params)
	}
	verifyClean(t, s2.Case)
}

func TestSlackUncheckedAssumptionStaysInProvenance(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}, {Path: "B.TXT", Data: slackFill(10)}}, 2)
	f.patchTail("A.TXT", false, 10)
	f.patchTail("B.TXT", false, 10)
	opens := map[string]int{}
	fake := &slackFakeFS{open: func(e filesys.Entry, in filesys.File) (filesys.File, error) {
		opens[e.Name]++
		if e.Name == "B.TXT" && opens[e.Name] == 2 { // the shared-unit pass cannot open it
			return nil, errors.New("second open fails")
		}
		return in, nil
	}}
	s := slackSession(t, f.img, fakeOpts(fake))
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
	bin := slackRecord(t, sum, "p0-fat16/slack.bin")
	if !bin.Incomplete || !slices.Contains(bin.Source.Derived.Recovery.Assumptions, slackUncheckedTag) {
		t.Fatalf("bin %+v assumptions %v", bin, bin.Source.Derived.Recovery.Assumptions)
	}
	if !strings.Contains(bin.Error, "could not be opened") {
		t.Errorf("error = %q", bin.Error)
	}
	verifyClean(t, s.Case)
}

func TestSlackNeverModifiesSource(t *testing.T) {
	_, img := fullFAT(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{})
	mustExportSlack(t, s, SlackOptions{Partition: -1})
	f, rec, err := c.OpenArtifact(s.Parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(h.Sum(nil)) != s.Parent.SHA256 || n != s.Parent.Size || rec.SHA256 != s.Parent.SHA256 {
		t.Errorf("the parent changed: %d bytes, sha %x, manifest %s", n, h.Sum(nil), s.Parent.SHA256)
	}
}

func TestSlackTwiceWritesTwoAnalyses(t *testing.T) {
	_, img := fullFAT(t)
	s := slackSession(t, img, Options{})
	a := mustExportSlack(t, s, SlackOptions{Partition: -1})
	b := mustExportSlack(t, s, SlackOptions{Partition: -1})
	if a.AnalysisID == b.AnalysisID {
		t.Fatal("two runs share an analysis id")
	}
	bins := 0
	paths := map[string]bool{}
	recs, err := s.Case.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Source.Kind == evidence.KindSlack {
			bins++
		}
		if paths[r.Path] {
			t.Errorf("path %s written twice", r.Path)
		}
		paths[r.Path] = true
	}
	if bins != 4 {
		t.Errorf("%d slack artifacts, want 4", bins)
	}
	verifyClean(t, s.Case)
}

func TestSlackRecoversPanic(t *testing.T) {
	f, _ := threeTails(t)
	fake := &slackFakeFS{readDir: func(_ filesys.Entry, _ []filesys.Entry, _ error) ([]filesys.Entry, error) { panic("hostile ReadDir") }}
	s := slackSession(t, f.img, fakeOpts(fake))
	sum, err := exportSlack(t, s, SlackOptions{Partition: -1, Files: true})
	if err != nil || sum.WalkErrors != 1 || !sum.Incomplete {
		t.Fatalf("err %v summary %+v", err, sum)
	}
	verifyClean(t, s.Case)
}

func TestExportSlackEscapesHostileLabels(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	f.patchTail("A.TXT", false, 10)
	fake := &slackFakeFS{readDir: func(_ filesys.Entry, kids []filesys.Entry, err error) ([]filesys.Entry, error) {
		for i := range kids {
			kids[i].Name = "a\xff\x00.txt"
		}
		return kids, err
	}}
	s := slackSession(t, f.img, fakeOpts(fake))
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
	lines := slackLinesOf(t, s.Case, slackRecord(t, sum, "slack.runs.jsonl"), evidence.MethodSlackFile)
	if len(lines) != 1 || lines[0].FSPath != `/a\xFF\x00.txt` || !lines[0].PathEscaped {
		t.Fatalf("lines = %+v", lines)
	}
	verifyClean(t, s.Case)
}
