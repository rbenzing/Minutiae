package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const (
	sha64a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha64b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func renderProv(p records.Provenance, row records.Row) string {
	var b bytes.Buffer
	printProvenance(&b, p, row)
	return b.String()
}

func requireLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out+"\n", l+"\n") {
			t.Errorf("no line %q in:\n%s", l, out)
		}
	}
}

func boundHop(a evidence.ManifestRecord, link records.LinkState, seq int64) records.Hop {
	return records.Hop{Artifact: a, Link: link, Audit: evidence.AuditBinding{State: evidence.AuditBound, Seq: seq}}
}

func twoHop() records.Provenance {
	img := evidence.ManifestRecord{
		ID: "img1", Path: "artifacts/dev1/acq1/disk.img", Size: 4096, SHA256: sha64a,
		Source: evidence.Source{Kind: "partition", DeviceID: "dev1", OriginalPath: `C:\disk.img`, Segment: 1, Segments: 3},
	}
	f := evidence.ManifestRecord{
		ID: "f1", Path: "artifacts/dev1/acq2/f.bin", Size: 100, SHA256: sha64b,
		Source: evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
			ParentID: "img1", ParentSHA256: sha64a, Partition: 2, PartitionOffset: 1048576, FSType: "ext4", FSPath: "/a/b", FSID: "ino:12",
			Runs: []evidence.Run{{Offset: 4096, Length: 100}},
		}},
	}
	ext := records.ImageExtent{ArtifactOffset: 10, Length: 20, ImageOffset: 4106}
	return records.Provenance{
		Chain:       []records.Hop{boundHop(f, records.LinkOK, 7), boundHop(img, records.LinkRoot, 3)},
		ReachedRoot: true,
		Offset: records.OffsetInfo{
			State: records.OffsetTranslated, Coordinates: "media", Segments: 3,
			Hops:  []records.OffsetHop{{From: "f1", To: "img1", Runs: "inline", Total: 1, Extents: []records.ImageExtent{ext}}},
			Image: []records.ImageExtent{ext},
		},
	}
}

func TestShowProvenanceLiveExtractedFile(t *testing.T) {
	out := renderProv(twoHop(), records.Row{})
	requireLines(t, out,
		"  provenance:",
		"    status:        live",
		"    [0] artifact   f1  artifacts/dev1/acq2/f.bin  100 bytes  sha256 "+sha64b,
		"        derived:   extract of partition 2 (ext4) at image offset 1048576, fs path /a/b, fs id ino:12",
		"        runs:      1 inline",
		"        link:      ok -> [1]",
		"        audit:     bound to audit seq 7",
		"    [1] artifact   img1  artifacts/dev1/acq1/disk.img  4096 bytes  sha256 "+sha64a,
		`        root:      partition from device dev1, original path C:\disk.img, segment 1 of 3`,
		"        audit:     bound to audit seq 3",
		"    image offset (logical media offset, 3 segment(s)):  bytes 10..30 of f1 = media bytes 4106..4126",
		"    checked:       manifest links and hashes, audit entries, runs sidecar hash; NOT checked here: file contents, image bytes at the runs, the audit chain (run: minutiae case verify)",
	)
	if !strings.HasPrefix(out, "  provenance:\n    status:") {
		t.Errorf("status is not the first line:\n%s", out)
	}
	if strings.Contains(out, "problems:") || strings.Contains(out, "notes:") {
		t.Errorf("clean provenance prints problems or notes:\n%s", out)
	}
}

func TestShowProvenanceRecoveredRendering(t *testing.T) {
	p := twoHop()
	conf := 60
	p.Chain[0].Artifact.Source.Kind = "recover"
	p.Chain[0].Link = records.LinkParentMissing // damaged chain
	p.Chain = p.Chain[:1]
	p.ReachedRoot = false
	p.Recovery = &records.RecoveryView{ArtifactID: "f1", Hops: 0, MinConfidence: &conf, Recovery: evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &conf,
		Basis: []string{"dentry:1:1:1"}, Assumptions: []string{"prefix-only"},
		Alloc:  evidence.AllocSummary{Free: 90, Allocated: 6, Unknown: 4, ExcludedRuns: 2, ExcludedBytes: 50},
		Params: map[string]string{"k": "v"},
	}}
	p.Problems = []records.Problem{{Kind: records.ProblemParentMissing, Hop: 0, Detail: `parent "img1" is not in the manifest`}}
	p.Offset = records.OffsetInfo{State: records.OffsetUnavailable, Coordinates: "media", Reason: "the provenance chain is broken"}
	out := renderProv(p, records.Row{Recovered: true, Method: "fat-contiguous"})
	if !strings.HasPrefix(out, "  provenance:\n    status:        RECOVERED DATA (class deleted-file, method fat-contiguous, confidence 60; not live evidence)\n") {
		t.Errorf("recovered status is not first:\n%s", out)
	}
	requireLines(t, out,
		"        link:      PARENT MISSING img1",
		"    recovery:      artifact f1 (0 hops up), basis dentry:1:1:1, assumptions prefix-only, params k=v, alloc 90/6/4, captured 100 bytes; 50 bytes in 2 runs named but not captured",
		"    image offset unavailable: the provenance chain is broken",
		`    problems:      PROBLEM parent-missing: parent "img1" is not in the manifest`,
	)
	// a live row planted on a recovered artifact is still introduced as recovered data
	if !strings.Contains(renderProv(p, records.Row{}), "RECOVERED DATA (class deleted-file") {
		t.Errorf("a live record on a recovered artifact must still read as recovered data")
	}
	// no confidence
	p.Recovery.MinConfidence, p.Recovery.Recovery.Confidence = nil, nil
	if !strings.Contains(renderProv(p, records.Row{}), "confidence none;") {
		t.Errorf("missing confidence is not printed as none")
	}
}

func TestShowProvenanceUnresolvedLinksAreVisible(t *testing.T) {
	mk := func(link records.LinkState, kind records.ProblemKind, detail string, mutate func(*records.Provenance)) string {
		p := twoHop()
		p.Chain = p.Chain[:1]
		p.Chain[0].Link = link
		p.ReachedRoot = false
		p.Problems = []records.Problem{{Kind: kind, Hop: 0, Detail: detail}}
		p.Offset = records.OffsetInfo{State: records.OffsetUnavailable, Coordinates: "media", Reason: "broken"}
		if mutate != nil {
			mutate(&p)
		}
		return renderProv(p, records.Row{})
	}
	requireLines(t, mk(records.LinkParentMissing, records.ProblemParentMissing, "gone", nil),
		"        link:      PARENT MISSING img1", "    problems:      PROBLEM parent-missing: gone")
	requireLines(t, mk(records.LinkParentAmbiguous, records.ProblemParentAmbiguous, "two", nil),
		"        link:      PARENT AMBIGUOUS", "    problems:      PROBLEM parent-ambiguous: two")
	requireLines(t, mk(records.LinkTooDeep, records.ProblemTooDeep, "deep", nil),
		"        link:      CHAIN TOO DEEP (more than 16 links)", "    problems:      PROBLEM too-deep: deep")

	// hash differs: the parent hop carries the flag; the child's link names both hashes
	hd := mk(records.LinkOK, records.ProblemParentHash, "differs", func(p *records.Provenance) {
		*p = twoHop()
		p.Chain[1].Link = records.LinkHashDiffers
		p.Chain[1].Artifact.SHA256 = sha64b
		p.Problems = []records.Problem{{Kind: records.ProblemParentHash, Hop: 1, Detail: "differs"}}
	})
	requireLines(t, hd, "        link:      PARENT HASH DIFFERS (recorded "+sha64a+", found "+sha64b+")", "    problems:      PROBLEM parent-hash: differs")

	// cycle: the second hop derives from the first
	cy := mk(records.LinkOK, records.ProblemCycle, "loop", func(p *records.Provenance) {
		*p = twoHop()
		p.Chain[1].Artifact.Source.Derived = &evidence.Derivation{ParentID: "f1", ParentSHA256: sha64b}
		p.Chain[1].Link = records.LinkCycle
		p.ReachedRoot = false
		p.Problems = []records.Problem{{Kind: records.ProblemCycle, Hop: 1, Detail: "loop"}}
	})
	requireLines(t, cy, "        link:      CYCLE (derives from [0])", "    problems:      PROBLEM cycle: loop")

	// unbound audit entry
	ub := mk(records.LinkOK, records.ProblemAuditDiffers, "x", func(p *records.Provenance) {
		*p = twoHop()
		p.Chain[0].Audit = evidence.AuditBinding{State: evidence.AuditDiffers, Detail: "source differs"}
	})
	requireLines(t, ub, "        audit:     NOT BOUND: differs: source differs")
}

func TestShowOffsetLines(t *testing.T) {
	base := func() records.Provenance { return twoHop() }
	single := renderProv(base(), records.Row{})
	requireLines(t, single, "    image offset (logical media offset, 3 segment(s)):  bytes 10..30 of f1 = media bytes 4106..4126")

	p := base()
	p.Offset.Image = []records.ImageExtent{{ArtifactOffset: 0, Length: 10, ImageOffset: 100}, {ArtifactOffset: 10, Length: 5, ImageOffset: -1, Hole: true}, {ArtifactOffset: 15, Length: 5, ImageOffset: 900}}
	two := renderProv(p, records.Row{})
	requireLines(t, two,
		"    image offset (logical media offset, 3 segment(s)):  bytes 0..10 of f1 = media bytes 100..110",
		"        bytes 10..15 of f1 = hole of 5 bytes (no media bytes)",
		"        bytes 15..20 of f1 = media bytes 900..905")

	p = base()
	p.Offset = records.OffsetInfo{State: records.OffsetUnavailable, Coordinates: "media", Reason: "artifact is not derived"}
	requireLines(t, renderProv(p, records.Row{}), "    image offset unavailable: artifact is not derived")

	p = base()
	p.Offset = records.OffsetInfo{State: records.OffsetNoRange, Coordinates: "media"}
	requireLines(t, renderProv(p, records.Row{}), "    image offset:  (record has no byte range)")

	// more than 20 extents: 20 printed, the rest counted; a truncated last hop says so
	p = base()
	p.Offset.Image = nil
	for i := range 25 {
		p.Offset.Image = append(p.Offset.Image, records.ImageExtent{ArtifactOffset: int64(i), Length: 1, ImageOffset: int64(2 * i)})
	}
	p.Offset.Hops[0].Total, p.Offset.Hops[0].Truncated = 300, true
	many := renderProv(p, records.Row{})
	requireLines(t, many, "        ... and 5 more extents", "        300 extents, first 256 listed")
	if strings.Contains(many, "bytes 20..21 of f1") {
		t.Errorf("more than 20 extents printed:\n%s", many)
	}

	// truncated mid-chain: the earlier hop line, the stopped hop's truncation and the reason
	p = base()
	p.Offset = records.OffsetInfo{
		State: records.OffsetUnavailable, Coordinates: "media", Reason: "more than 256 fragments at f1; translation stopped",
		Hops: []records.OffsetHop{
			{From: "g1", To: "f1", Runs: "sidecar rs1", Total: 2, Extents: []records.ImageExtent{{ArtifactOffset: 0, Length: 4, ImageOffset: 8}, {ArtifactOffset: 4, Length: 4, ImageOffset: 40}}},
			{From: "f1", To: "img1", Runs: "inline", Total: 300, Truncated: true},
		},
	}
	mid := renderProv(p, records.Row{})
	requireLines(t, mid,
		"    image offset unavailable: more than 256 fragments at f1; translation stopped",
		"        via g1 -> f1 (sidecar rs1): 2 extents",
		"          bytes 0..4 = media bytes 8..12",
		"          bytes 4..8 = media bytes 40..44",
		"        300 extents, first 256 listed")

	// the incomplete note on the offset block
	p = base()
	p.Offset.Hops[0].RunsExceedSize = true
	requireLines(t, renderProv(p, records.Row{}), "        runs describe more than the bytes held (incomplete)")

	// segment label when the count is not recorded
	p = base()
	p.Offset.Segments = 0
	requireLines(t, renderProv(p, records.Row{}), "    image offset (logical media offset, segments not recorded):  bytes 10..30 of f1 = media bytes 4106..4126")
}

func TestShowDeclaredRunsLine(t *testing.T) {
	for _, st := range []string{"ok", "missing", "not-runs-sidecar", "other-parent", "other-directory", "multiply-referenced", "used-as-captured"} {
		p := twoHop()
		p.Recovery = &records.RecoveryView{
			ArtifactID: "f1", Recovery: evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "m"},
			DeclaredRuns: &records.DeclaredRunsView{ArtifactID: "decl1", State: st},
		}
		requireLines(t, renderProv(p, records.Row{}), "    declared runs: sidecar decl1 (declared by the filesystem, not captured, not used for offsets) ["+st+"]")
	}
	p := twoHop()
	p.Recovery = &records.RecoveryView{
		ArtifactID: "f1", Recovery: evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "m"},
		DeclaredRuns: &records.DeclaredRunsView{ArtifactID: "decl1", State: "missing", Detail: "not in manifest"},
	}
	requireLines(t, renderProv(p, records.Row{}), "    declared runs: sidecar decl1 (declared by the filesystem, not captured, not used for offsets) [missing: not in manifest]")
	if strings.Contains(renderProv(twoHop(), records.Row{}), "declared runs:") {
		t.Error("declared runs line printed without a declared list")
	}
}

func TestShowProvenanceProblemsAreCappedAndNotesPrinted(t *testing.T) {
	p := twoHop()
	p.Problems = []records.Problem{{Kind: records.ProblemSegment, Hop: 0, Detail: "one"}, {Kind: records.ProblemSegment, Hop: 0, Detail: "two"}}
	p.ProblemsSuppressed = 7
	p.Notes = []string{"first note", "second note"}
	out := renderProv(p, records.Row{})
	requireLines(t, out,
		"    problems:      PROBLEM segment: one",
		"                   PROBLEM segment: two",
		"                   7 further problems not listed",
		"    notes:         first note",
		"                   second note")
}

func TestShowProvenanceSegmentsLine(t *testing.T) {
	p := twoHop()
	p.Chain[0].SegmentsTotal, p.Chain[0].SegmentsBad = 30, 2
	p.Chain[0].Segments = []records.SegmentCheck{{SegmentRef: evidence.SegmentRef{ID: "s1"}, State: "ok"}, {SegmentRef: evidence.SegmentRef{ID: "s2"}, State: "missing"}}
	requireLines(t, renderProv(p, records.Row{}),
		"        parent segments: 30 listed, 2 bad (first 20 shown)",
		"          segment 2: s2 missing")
}

func TestShowProvenanceEscapesEverything(t *testing.T) {
	evil := func(tag string) string { return tag + "\nFAKE: line\x1b[31m\u202e\xff\u0085" }
	p := twoHop()
	conf := 10
	p.Chain[0].Artifact.ID = evil("id")
	p.Chain[0].Artifact.Path = evil("path")
	p.Chain[0].Artifact.Incomplete, p.Chain[0].Artifact.Error = true, evil("err")
	d := p.Chain[0].Artifact.Source.Derived
	d.FSPath, d.FSID, d.FSType, d.ParentID = evil("fspath"), evil("fsid"), evil("fstype"), evil("parent")
	d.RunsArtifact, d.Runs = evil("sidecar"), nil
	d.Snapshot = &evidence.SnapshotRef{Name: evil("snap"), Xid: 9}
	p.Chain[1].Artifact.Source.OriginalPath, p.Chain[1].Artifact.Source.RemotePath = evil("orig"), evil("remote")
	p.Chain[1].Artifact.Source.DeviceID, p.Chain[1].Artifact.Source.Kind = evil("dev"), evil("kind")
	p.Chain[0].Segments = []records.SegmentCheck{{SegmentRef: evidence.SegmentRef{ID: evil("seg")}, State: "missing"}}
	p.Chain[0].SegmentsTotal, p.Chain[0].SegmentsBad = 1, 1
	p.Chain[0].Audit = evidence.AuditBinding{State: evidence.AuditState(evil("auditstate")), Detail: evil("audit")}
	p.Chain[0].Artifact.SHA256 = evil("hopsha")
	d.ParentSHA256 = evil("recordedsha")
	p.Chain[1].Link = records.LinkHashDiffers
	p.Chain[1].Artifact.SHA256 = evil("foundsha")
	p.Chain[0].Segments = append(p.Chain[0].Segments, records.SegmentCheck{SegmentRef: evidence.SegmentRef{ID: "s9"}, State: evil("segstate")})
	p.Chain[0].SegmentsTotal, p.Chain[0].SegmentsBad = 2, 2
	p.Offset.Hops = []records.OffsetHop{
		{From: evil("hopfrom"), To: evil("hopto"), Runs: evil("hopruns"), Total: 1},
		{From: "f1", To: "img1", Runs: "inline", Total: 1},
	}
	p.Recovery = &records.RecoveryView{
		ArtifactID: evil("recid"), MinConfidence: &conf, Recovery: evidence.Recovery{
			Class: evil("class"), Method: evil("method"), Basis: []string{evil("basis")}, Assumptions: []string{evil("assume")},
			Params: map[string]string{evil("pk"): evil("pv")},
		},
		DeclaredRuns: &records.DeclaredRunsView{ArtifactID: evil("decl"), State: evil("state"), Detail: evil("detail")},
	}
	p.Problems = []records.Problem{{Kind: records.ProblemKind(evil("problemkind")), Hop: 0, Detail: evil("problem")}}
	p.Notes = []string{evil("note")}
	p.Offset.Reason = evil("reason")
	p.Offset.State = records.OffsetUnavailable
	out := renderProv(p, records.Row{})
	noRawNonPrintable(t, "provenance", out)
	if strings.Contains(out, "FAKE: line\x1b") || strings.Contains(out, "\nFAKE:") {
		t.Errorf("injected line survived:\n%s", out)
	}
	// the quoted/escaped forms appear
	for _, want := range []string{`"fspath\nFAKE: line\x1b[31m\u202e\xff\u0085"`, `snapshot "snap\x0aFAKE: line\x1b[31m\u202e\xff\x85" (xid 9)`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing escaped form %s in:\n%s", want, out)
		}
	}
	// the non-printable-free output is also one logical line per label: no line starts with FAKE
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "FAKE") {
			t.Errorf("forged line %q", l)
		}
	}
}

func TestShowProvenanceRowRecoveredWithoutRecoveryView(t *testing.T) {
	c := 40
	out := renderProv(records.Provenance{}, records.Row{Recovered: true, Method: "carve", Confidence: &c})
	if !strings.HasPrefix(out, "  provenance:\n    status:        RECOVERED DATA (class unknown, method carve, confidence 40; not live evidence)\n") {
		t.Errorf("a recovered row without a recovery view must read as recovered data, class unknown:\n%s", out)
	}
	out = renderProv(records.Provenance{}, records.Row{Recovered: true, Method: "carve"})
	requireLines(t, out, "    status:        RECOVERED DATA (class unknown, method carve, confidence none; not live evidence)")
	if !strings.Contains(renderProv(records.Provenance{ReachedRoot: true}, records.Row{}), "status:        live\n") {
		t.Error("a live row must read as live")
	}
}

func TestShowProvenancePrintsLimitsLine(t *testing.T) {
	for name, p := range map[string]records.Provenance{
		"empty":     {},
		"two hops":  twoHop(),
		"no chain":  {Problems: []records.Problem{{Kind: records.ProblemParentMissing, Hop: -1, Detail: "x"}}},
		"recovered": {Recovery: &records.RecoveryView{ArtifactID: "r", Recovery: evidence.Recovery{Class: "c", Method: "m"}}},
	} {
		out := renderProv(p, records.Row{})
		if !strings.Contains(out, "    checked:       manifest links and hashes, audit entries, runs sidecar hash; NOT checked here: file contents, image bytes at the runs, the audit chain (run: minutiae case verify)\n") {
			t.Errorf("%s: no checked line:\n%s", name, out)
		}
		if !strings.HasPrefix(out, "  provenance:\n    status:") {
			t.Errorf("%s: status not first:\n%s", name, out)
		}
	}
}

// End to end through the command: a two-hop chain of a live record, and a recovered record whose
// chain is damaged.
func TestShowProvenanceThroughTheCommand(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	f := recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5",
		Runs: []evidence.Run{{Offset: 100, Length: 8}},
	}}, []byte("abcdefgh"))
	rec := recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), rptr(60))
	recordstest.Ingest(t, c, recParser, []string{f.ID}, []records.Record{{
		Type: "note", ArtifactID: f.ID, Summary: "live note", Payload: map[string]any{}, Range: &records.Range{Offset: 2, Length: 4},
	}})
	recordstest.Ingest(t, c, records.Parser{Name: "p2", Version: "1"}, []string{rec.ID}, []records.Record{{
		Type: "note", ArtifactID: rec.ID, Summary: "recovered note", Payload: map[string]any{}, Deleted: true, Recovery: "fat-contiguous", Confidence: rptr(50),
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "records", "show", "--case", dir, "1")
	if code != 0 {
		t.Fatalf("show: %d\n%s", code, out)
	}
	requireLines(t, out,
		"    status:        live",
		"    [0] artifact   "+f.ID+"  "+f.Path+"  8 bytes  sha256 "+f.SHA256,
		"        derived:   extract of partition 1 (mtfs) at image offset 0, fs path /f, fs id nid:5",
		"        runs:      1 inline",
		"        link:      ok -> [1]",
		"    image offset (logical media offset, "+"segments not recorded):  bytes 2..6 of "+f.ID+" = media bytes 102..106",
	)
	if !strings.Contains(out, "source:") {
		t.Errorf("the source line is gone:\n%s", out)
	}
	code, out = run(t, Deps{}, "records", "show", "--case", dir, "2")
	if code != 0 {
		t.Fatalf("show 2: %d\n%s", code, out)
	}
	if !strings.Contains(out, "    status:        RECOVERED DATA (class ") || !strings.Contains(out, "not live evidence)") {
		t.Errorf("recovered status missing:\n%s", out)
	}
	if strings.Index(out, "status:") > strings.Index(out, "checked:") {
		t.Errorf("status not before checked")
	}
}

// E43: a LIVE record planted on a recovered artifact (a forged row) is still introduced as recovered
// data, first, by the real command.
func TestShowProvenanceRecoveredStatusFirst(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	rec := recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), rptr(60))
	recordstest.Ingest(t, c, recParser, []string{rec.ID}, []records.Record{{
		Type: "note", ArtifactID: rec.ID, Summary: "recovered note", Payload: map[string]any{}, Deleted: true, Recovery: "fat-contiguous", Confidence: rptr(50),
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.InjectRecord(t, dir, 99, 1, rec.ID, "planted live")
	_, out := run(t, Deps{}, "records", "show", "--case", dir, "99")
	if !strings.Contains(out, "record 99\n") {
		t.Fatalf("the planted record was not shown:\n%s", out)
	}
	if !strings.Contains(out, "  provenance:\n    status:        RECOVERED DATA (class ") || !strings.Contains(out, "not live evidence)") {
		t.Errorf("a live record on a recovered artifact must read RECOVERED DATA first:\n%s", out)
	}
	if strings.Contains(out, "status:        live") {
		t.Errorf("planted record read as live:\n%s", out)
	}
}
