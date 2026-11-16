package examine_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// R13 (3G-R18): case verify recomputes every ext4 journal artifact from the image.

// jvCase is a case holding the jbd2 report, the block copies of transaction 10 and the file recovered from its inode
// copy (the image of recover_ext4_journal_test.go: one committed transaction, live region).
type jvCase struct {
	s      *examine.Session
	disk   []byte
	img    jrnImg
	report evidence.ManifestRecord
	blocks []evidence.ManifestRecord
	file   evidence.ManifestRecord
	start  int64
}

func newJVCase(t *testing.T, spec jrnSpec) *jvCase {
	t.Helper()
	j := buildJrn(t, spec)
	s, disk := imageSession(t, j.img)
	part, err := s.Partition(-1)
	if err != nil {
		t.Fatal(err)
	}
	jc := &jvCase{s: s, disk: disk, img: j, start: part.Start}
	_, jc.report = jbReport(t, s, jo, 0)
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 10})
	if err != nil || len(sum.Artifacts) == 0 {
		t.Fatalf("JournalBlocks: %+v, %v", sum, err)
	}
	jc.blocks = sum.Artifacts
	use := filesys.JournalUseCommitted
	if spec.clean {
		use = filesys.JournalUseStale
	}
	jc.file = only(t, recoverJournal(t, s, use).Artifacts)
	return jc
}

func jvProblems(rep evidence.VerifyReport) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, "journal: ") {
			out = append(out, p)
		}
	}
	return out
}

// clone writes a new artifact with the bytes and derivation of rec (so every other check still passes) after mut
// changed the derivation, as a writer with a bug would.
func (jc *jvCase) clone(t *testing.T, rec evidence.ManifestRecord, path string, mut func(d *evidence.Derivation)) evidence.ManifestRecord {
	t.Helper()
	d := *rec.Source.Derived
	d.Runs = slices.Clone(d.Runs)
	rv := *d.Recovery
	rv.Basis = slices.Clone(rv.Basis)
	rv.Assumptions = slices.Clone(rv.Assumptions)
	if rv.Journal != nil {
		j := *rv.Journal
		rv.Journal = &j
	}
	params := map[string]string{}
	for k, v := range rv.Params {
		params[k] = v
	}
	rv.Params = params
	d.Recovery = &rv
	mut(&d)
	data := readArtifact(t, jc.s.Case, rec)
	p := jc.s.Parent
	out, err := jc.s.Case.Capture(p.Source.DeviceID, "forged1", path, evidence.Source{Kind: rec.Source.Kind, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyJournalClean(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	rep := rfVerify(t, jc.s.Case)
	if !rep.OK() {
		t.Fatalf("problems: %v", rep.Problems)
	}
	want := 1 + len(jc.blocks) + 1
	if rep.JournalRechecked != want {
		t.Errorf("JournalRechecked = %d, want %d", rep.JournalRechecked, want)
	}
	if rep.RollForwardRechecked != 0 {
		t.Errorf("RollForwardRechecked = %d for an ext4 case", rep.RollForwardRechecked)
	}
	for _, n := range rep.Notices {
		if strings.Contains(n, `kind "report"`) {
			t.Errorf("a report R13 rendered again still has its notice: %s", n)
		}
	}
	runs := auditByAction(t, jc.s.Case, "verify.run")
	if got := runs[len(runs)-1].Details["journal_rechecked"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("verify.run journal_rechecked = %v, want %d", got, want)
	}
	// without R13 the report is noticed, not reproduced
	plain, err := jc.s.Case.VerifyWith(context.Background(), examine.RecoveredCheck(), examine.AllocationCheck(nil), examine.GapCheck())
	if err != nil {
		t.Fatal(err)
	}
	if plain.JournalRechecked != 0 {
		t.Errorf("checks without R13 counted %d", plain.JournalRechecked)
	}
	evidencetest.RequireNotice(t, plain, `kind "report"`)
}

func TestVerifyJournalNoStaleReport(t *testing.T) {
	// a report written with --no-stale is rendered again the same way (params stale=off)
	j := buildJrn(t, jrnSpec{clean: true})
	s, _ := imageSession(t, j.img)
	jbReport(t, s, examine.JournalOptions{Partition: -1, NoStale: true}, 0)
	rep := rfVerify(t, s.Case)
	if !rep.OK() || rep.JournalRechecked != 1 {
		t.Fatalf("problems %v, rechecked %d", rep.Problems, rep.JournalRechecked)
	}
}

func TestVerifyJournalReportRewrittenConsistently(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	forged := []byte(`{"type":"journal","format":"minutiae-ext4-journal/1","partition":1}` + "\n")
	recordstest.RewriteArtifactConsistently(t, jc.s.Case.Dir, jc.report.ID, forged)
	rep := rfVerify(t, jc.s.Case)
	got := jvProblems(rep)
	if len(got) != 1 || !strings.Contains(got[0], "report differs from a fresh rendering") {
		t.Fatalf("journal problems %q (all: %v)", got, rep.Problems)
	}
	if rep.OK() {
		t.Error("a forged report verified")
	}
}

func TestVerifyJournalReportIncompleteFlagDiffers(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	evidencetest.RewriteManifest(t, jc.s.Case.Dir, jc.report.ID, func(r *evidence.ManifestRecord) {
		r.Incomplete, r.Error = true, "claimed"
	})
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "recorded incomplete=true") {
		t.Fatalf("journal problems %q", got)
	}
}

// A report R13 cannot render again keeps the kind-report notice (and a problem says why).
func TestVerifyJournalUnrecomputedReportKeepsNotice(t *testing.T) {
	for name, mut := range map[string]func(r *evidence.Recovery){
		"no max_bytes":   func(r *evidence.Recovery) { delete(r.Params, "max_bytes") },
		"bad stale":      func(r *evidence.Recovery) { r.Params["stale"] = "maybe" },
		"another format": func(r *evidence.Recovery) { r.Params["format"] = "minutiae-ext4-journal/9" },
	} {
		t.Run(name, func(t *testing.T) {
			jc := newJVCase(t, jrnSpec{})
			// the real report is rendered again and marked; the crafted one is not
			jc.clone(t, jc.report, "reports/p1-ext4/journal-2.jsonl", func(d *evidence.Derivation) { mut(d.Recovery) })
			rep := rfVerify(t, jc.s.Case)
			if got := jvProblems(rep); len(got) != 1 || !strings.Contains(got[0], "report") {
				t.Fatalf("journal problems %q", got)
			}
			evidencetest.RequireNotice(t, rep, `kind "report": 1 artifact(s)`)
		})
	}
}

func TestVerifyJournalWrongPartitionOffsetKeepsNotice(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.report, "reports/p1-ext4/journal-2.jsonl", func(d *evidence.Derivation) { d.PartitionOffset += 512 })
	rep := rfVerify(t, jc.s.Case)
	if got := jvProblems(rep); len(got) != 1 || !strings.Contains(got[0], "starts at") {
		t.Fatalf("journal problems %q", got)
	}
	evidencetest.RequireNotice(t, rep, `kind "report": 1 artifact(s)`)
}

func TestVerifyJournalCraftedMethodIsAProblem(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.report, "reports/p1-ext4/journal-2.jsonl", func(d *evidence.Derivation) { d.Recovery.Method = "ext4-journal-bogus" })
	rep := rfVerify(t, jc.s.Case)
	if got := jvProblems(rep); len(got) != 1 || !strings.Contains(got[0], "not an ext4 journal artifact") {
		t.Fatalf("journal problems %q", got)
	}
	evidencetest.RequireNotice(t, rep, `kind "report": 1 artifact(s)`)
}

func TestVerifyJournalBlockNotATag(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk999.bin", func(d *evidence.Derivation) {
		for i, b := range d.Recovery.Basis {
			if strings.HasPrefix(b, "fsblk=") {
				d.Recovery.Basis[i] = "fsblk=999"
			}
		}
	})
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "names no tag") {
		t.Fatalf("journal problems %q", got)
	}
}

func TestVerifyJournalBlockRefDiffers(t *testing.T) {
	for name, mut := range map[string]func(j *evidence.JournalRef){
		"committed": func(j *evidence.JournalRef) { j.Committed = !j.Committed },
		"revoked":   func(j *evidence.JournalRef) { j.Revoked = !j.Revoked },
		"region":    func(j *evidence.JournalRef) { j.Region = "stale" },
	} {
		t.Run(name, func(t *testing.T) {
			jc := newJVCase(t, jrnSpec{})
			jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-x.bin", func(d *evidence.Derivation) { mut(d.Recovery.Journal) })
			got := jvProblems(rfVerify(t, jc.s.Case))
			if len(got) != 1 || !strings.Contains(got[0], "journal reference") {
				t.Fatalf("journal problems %q", got)
			}
		})
	}
}

func TestVerifyJournalBlockRunDiffers(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-x.bin", func(d *evidence.Derivation) {
		d.Runs[0].Offset += 1024 // the next slot of the log: not this copy's
	})
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) == 0 || !strings.Contains(got[0], "run") {
		t.Fatalf("journal problems %q", got)
	}
}

// Ruling C17: the unknown-bitmap marker is recomputed from a fresh view, both ways.
func TestVerifyJournalBitmapMarker(t *testing.T) {
	strip := func(d *evidence.Derivation) {
		d.Recovery.Basis = slices.DeleteFunc(d.Recovery.Basis, func(s string) bool { return s == "alloc-basis=journal-inode" })
		d.Recovery.Assumptions = slices.DeleteFunc(d.Recovery.Assumptions, func(s string) bool { return s == "bitmap-unconfirmed" })
	}
	add := func(d *evidence.Derivation) {
		strip(d)
		d.Recovery.Basis = append(d.Recovery.Basis, "alloc-basis=journal-inode")
		d.Recovery.Assumptions = append(d.Recovery.Assumptions, "bitmap-unconfirmed")
	}
	t.Run("unclean image, marker dropped", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{unclean: true})
		if !slices.Contains(recoveryOf(jc.blocks[0]).Assumptions, "bitmap-unconfirmed") {
			t.Fatal("the writer did not mark the copy on an unclean image")
		}
		jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-x.bin", strip)
		got := jvProblems(rfVerify(t, jc.s.Case))
		if len(got) != 1 || !strings.Contains(got[0], "unconfirmed") {
			t.Fatalf("journal problems %q", got)
		}
	})
	t.Run("clean image, marker invented", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{})
		if slices.Contains(recoveryOf(jc.blocks[0]).Assumptions, "bitmap-unconfirmed") {
			t.Fatal("the writer marked the copy on a clean image")
		}
		jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-x.bin", add)
		got := jvProblems(rfVerify(t, jc.s.Case))
		if len(got) != 1 || !strings.Contains(got[0], "unconfirmed") {
			t.Fatalf("journal problems %q", got)
		}
	})
	t.Run("half a marker", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{unclean: true})
		jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-x.bin", func(d *evidence.Derivation) {
			d.Recovery.Assumptions = slices.DeleteFunc(d.Recovery.Assumptions, func(s string) bool { return s == "bitmap-unconfirmed" })
		})
		if got := jvProblems(rfVerify(t, jc.s.Case)); len(got) != 1 {
			t.Fatalf("journal problems %q", got)
		}
	})
}

// Ruling C15: a copy whose log slot the bitmap calls free is a problem, however it came to be written.
func TestVerifyJournalBlockSlotInFreeSpace(t *testing.T) {
	first := newJVCase(t, jrnSpec{})
	r := first.blocks[0].Source.Derived.Runs[0]
	slot := int((r.Offset - first.start) / 1024)
	freed := buildJrn(t, jrnSpec{post: func(img []byte, l *ext4test.Layout) { l.SetBlockFree(img, slot, true) }})
	s, _ := imageSession(t, freed.img)
	jc := &jvCase{s: s}
	n, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 10})
	if err != nil || n.NotAllocated == 0 {
		t.Fatalf("the writer did not refuse the freed slot: %+v, %v", n, err)
	}
	for _, b := range n.Artifacts {
		if (b.Source.Derived.Runs[0].Offset-first.start)/1024 == int64(slot) {
			t.Fatal("the writer copied a slot the bitmap calls free")
		}
	}
	// craft the copy the writer would have made, with the bytes of the other case's copy
	data := readArtifact(t, first.s.Case, first.blocks[0])
	d := *first.blocks[0].Source.Derived
	d.Runs = slices.Clone(d.Runs)
	p := jc.s.Parent
	d.ParentID, d.ParentSHA256 = p.ID, p.SHA256
	if _, err := jc.s.Case.Capture(p.Source.DeviceID, "forged1", "journal/p1-ext4/seq10-fsblk1-x.bin", evidence.Source{Kind: evidence.KindJournal, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error { _, err := w.Write(data); return err }); err != nil {
		t.Fatal(err)
	}
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "free") {
		t.Fatalf("journal problems %q", got)
	}
}

func TestVerifyJournalInodeArtifactNotReproducible(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.file, "recovered/p1-ext4/000099-gone.bin", func(d *evidence.Derivation) { d.Runs[0].Offset += 1024 })
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "not a prefix") {
		t.Fatalf("journal problems %q", got)
	}
}

func TestVerifyJournalInodeRegionDiffers(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.file, "recovered/p1-ext4/000099-gone.bin", func(d *evidence.Derivation) { d.Recovery.Journal.Region = "stale" })
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "region") {
		t.Fatalf("journal problems %q", got)
	}
}

func TestVerifyJournalInodeSeqDiffers(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.file, "recovered/p1-ext4/000098-gone.bin", func(d *evidence.Derivation) { d.Recovery.Journal.Seq++ })
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "sequence") {
		t.Fatalf("journal problems %q", got)
	}
}

// The recorded size is short of the fresh candidate's and does not declare it.
func TestVerifyJournalInodeSizeShortWithoutDeclaration(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	rec := jc.file
	short := bytes.Clone(readArtifact(t, jc.s.Case, rec))[:1024]
	p := jc.s.Parent
	d := *rec.Source.Derived
	d.Runs = []evidence.Run{{Offset: rec.Source.Derived.Runs[0].Offset, Length: 1024}}
	rv := *d.Recovery
	d.Recovery = &rv
	if _, err := jc.s.Case.Capture(p.Source.DeviceID, "forged1", "recovered/p1-ext4/000097-gone.bin", evidence.Source{Kind: rec.Source.Kind, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error { _, err := w.Write(short); return err }); err != nil {
		t.Fatal(err)
	}
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "size") {
		t.Fatalf("journal problems %q", got)
	}
}

// An ext4-journal-inode file records the journal use it was recovered under; without it nothing can be recomputed.
func TestVerifyJournalInodeNeedsUseJournal(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	jc.clone(t, jc.file, "recovered/p1-ext4/000099-gone.bin", func(d *evidence.Derivation) { delete(d.Recovery.Params, "use_journal") })
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "use_journal") {
		t.Fatalf("journal problems %q", got)
	}
}

func TestVerifyJournalStaleFile(t *testing.T) {
	jc := newJVCase(t, jrnSpec{clean: true})
	rep := rfVerify(t, jc.s.Case)
	if want := 1 + len(jc.blocks) + 1; !rep.OK() || rep.JournalRechecked != want {
		t.Fatalf("problems %v, rechecked %d, want %d", rep.Problems, rep.JournalRechecked, want)
	}
}

func TestVerifyChecksIncludeJournalLast(t *testing.T) {
	checks := examine.VerifyChecks(examine.Options{})
	if len(checks) != 6 {
		t.Fatalf("%d composed checks, want 6", len(checks))
	}
	jc := newJVCase(t, jrnSpec{})
	rep, err := jc.s.Case.VerifyWith(context.Background(), checks[len(checks)-1])
	if err != nil {
		t.Fatal(err)
	}
	if rep.JournalRechecked == 0 {
		t.Error("the last composed check did not recompute any ext4 journal artifact")
	}
	rep, err = jc.s.Case.VerifyWith(context.Background(), checks[:len(checks)-1]...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.JournalRechecked != 0 {
		t.Error("a check before the last recomputed ext4 journal artifacts")
	}
}

// R13 leaves the F2FS artifacts to R12.
func TestVerifyJournalIgnoresRollForwardArtifacts(t *testing.T) {
	rc := newRFCase(t)
	rep := rfVerify(t, rc.s.Case)
	if !rep.OK() || rep.JournalRechecked != 0 || rep.RollForwardRechecked == 0 {
		t.Fatalf("problems %v, journal %d, rollforward %d", rep.Problems, rep.JournalRechecked, rep.RollForwardRechecked)
	}
}

func TestVerifyJournalProblemListIsBounded(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	for i := range 60 {
		jc.clone(t, jc.blocks[0], fmt.Sprintf("journal/p1-ext4/seq10-fsblk1-%d.bin", i), func(d *evidence.Derivation) { d.Recovery.Journal.Committed = !d.Recovery.Journal.Committed })
	}
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 51 || !strings.Contains(got[50], "10 further problems are not listed") {
		t.Fatalf("%d journal problems, last %q", len(got), got[len(got)-1])
	}
}

// cutCopy writes a block copy of rec as a writer that read only n bytes of the slot would have: the first n bytes, a
// run of n (none for 0), the marker read-failed-after=mark, Allocated n; incomplete says whether the artifact is
// flagged. mut may forge more.
func (jc *jvCase) cutCopy(t *testing.T, rec evidence.ManifestRecord, path string, n int, mark string, incomplete bool, mut func(d *evidence.Derivation)) evidence.ManifestRecord {
	t.Helper()
	d := *rec.Source.Derived
	rv := *d.Recovery
	rv.Assumptions = append(slices.Clone(rv.Assumptions), "read-failed-after="+mark)
	rv.Alloc.Allocated = int64(n)
	d.Recovery = &rv
	d.Runs = nil
	if n > 0 {
		d.Runs = []evidence.Run{{Offset: rec.Source.Derived.Runs[0].Offset, Length: int64(n)}}
	}
	if mut != nil {
		mut(&d)
	}
	data := readArtifact(t, jc.s.Case, rec)[:n]
	p := jc.s.Parent
	out, _ := jc.s.Case.Capture(p.Source.DeviceID, "forged1", path, evidence.Source{Kind: rec.Source.Kind, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error {
			if _, err := w.Write(data); err != nil {
				return err
			}
			if incomplete {
				return io.ErrUnexpectedEOF
			}
			return nil
		})
	return out
}

// A copy that claims a failed read is held to it: the slot is read again, and the marker, size, run, flag and
// allocation must agree.
func TestVerifyJournalBlockPartialCopy(t *testing.T) {
	const p = "journal/p1-ext4/seq10-fsblk1-cut.bin"
	for name, tc := range map[string]struct {
		n          int
		mark       string
		incomplete bool
		mut        func(d *evidence.Derivation)
		want       string
	}{
		"readable slot, forged marker and short run": {512, "512", true, nil, "readable"},
		"incomplete flag cleared":                    {512, "512", false, nil, "incomplete"},
		"marker disagrees with the run":              {512, "256", true, nil, "read-failed-after"},
		"marker not canonical":                       {512, "0512", true, nil, "read-failed-after"},
		"zero run, forged size":                      {0, "0", false, nil, "read-failed-after"},
		"marker as long as the slot":                 {1024, "1024", true, nil, "read-failed-after"},
		"allocation disagrees":                       {512, "512", true, func(d *evidence.Derivation) { d.Recovery.Alloc.Allocated = 1024 }, "allocated"},
	} {
		t.Run(name, func(t *testing.T) {
			jc := newJVCase(t, jrnSpec{})
			if name == "zero run, forged size" { // the artifact holds the whole slot but records no run
				jc.clone(t, jc.blocks[0], p, func(d *evidence.Derivation) {
					d.Runs = nil
					d.Recovery.Assumptions = append(d.Recovery.Assumptions, "read-failed-after=0")
					d.Recovery.Alloc.Allocated = 0
				})
			} else {
				jc.cutCopy(t, jc.blocks[0], p, tc.n, tc.mark, tc.incomplete, tc.mut)
			}
			got := jvProblems(rfVerify(t, jc.s.Case))
			if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Fatalf("journal problems %q, want one naming %q", got, tc.want)
			}
		})
	}
}

// Without the marker a copy is the whole slot, complete.
func TestVerifyJournalBlockWholeCopyIsComplete(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	rec := jc.blocks[0]
	d := *rec.Source.Derived
	rv := *d.Recovery
	rv.Basis = slices.Clone(rv.Basis)
	d.Recovery = &rv
	data := readArtifact(t, jc.s.Case, rec)
	p := jc.s.Parent
	_, _ = jc.s.Case.Capture(p.Source.DeviceID, "forged1", "journal/p1-ext4/seq10-fsblk1-inc.bin", evidence.Source{Kind: rec.Source.Kind, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error { _, _ = w.Write(data); return io.ErrUnexpectedEOF })
	got := jvProblems(rfVerify(t, jc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "incomplete") {
		t.Fatalf("journal problems %q", got)
	}
}

// A genuine partial copy, whose slot cannot be read to the end, stays clean.
func TestVerifyJournalBlockGenuinePartialCopyIsClean(t *testing.T) {
	for _, n := range []int{512, 0} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			jc := newJVCase(t, jrnSpec{})
			jc.cutCopy(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-cut.bin", n, fmt.Sprint(n), true, nil)
			defer examine.SetJournalSlotReader(func(_ io.ReaderAt, _, _ int64) (int64, error) {
				return int64(n), io.ErrUnexpectedEOF
			})()
			rep := rfVerify(t, jc.s.Case)
			if got := jvProblems(rep); len(got) != 0 {
				t.Fatalf("journal problems %q", got)
			}
			if want := 1 + len(jc.blocks) + 1 + 1; rep.JournalRechecked != want {
				t.Errorf("JournalRechecked = %d, want %d", rep.JournalRechecked, want)
			}
		})
	}
}

// A journal method on a kind R13 does not recompute is a problem, not silence.
func TestVerifyJournalMethodOnOtherKindIsAProblem(t *testing.T) {
	jc := newJVCase(t, jrnSpec{})
	rec := jc.blocks[0]
	d := *rec.Source.Derived
	p := jc.s.Parent
	if _, err := jc.s.Case.Capture(p.Source.DeviceID, "forged1", "carved/x.bin", evidence.Source{Kind: evidence.KindCarve, DeviceID: p.Source.DeviceID, Derived: &d},
		func(w io.Writer) error { _, err := w.Write(readArtifact(t, jc.s.Case, rec)); return err }); err != nil {
		t.Fatal(err)
	}
	got := jvProblems(rfVerify(t, jc.s.Case))
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "not an ext4 journal artifact") }) {
		t.Fatalf("journal problems %q", got)
	}
}

// R13 compares the scope of a journal report and a block copy with the one the writer stores, and the committed flag
// of a journal-derived file with the fresh candidate (3G whole-branch review).
func TestVerifyJournalScopeAndCommittedFlagsDiffer(t *testing.T) {
	t.Run("report scope", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{})
		evidencetest.RewriteManifest(t, jc.s.Case.Dir, jc.report.ID, func(r *evidence.ManifestRecord) { r.Source.Derived.Recovery.Scope = "unallocated" })
		got := jvProblems(rfVerify(t, jc.s.Case))
		if len(got) != 1 || !strings.Contains(got[0], "scope") {
			t.Fatalf("journal problems %q", got)
		}
	})
	t.Run("block scope", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{})
		jc.clone(t, jc.blocks[0], "journal/p1-ext4/seq10-fsblk1-scope.bin", func(d *evidence.Derivation) { d.Recovery.Scope = "unallocated" })
		got := jvProblems(rfVerify(t, jc.s.Case))
		if len(got) != 1 || !strings.Contains(got[0], "scope") {
			t.Fatalf("journal problems %q", got)
		}
	})
	t.Run("file committed", func(t *testing.T) {
		jc := newJVCase(t, jrnSpec{})
		jc.clone(t, jc.file, "recovered/p1-ext4/000097-gone.bin", func(d *evidence.Derivation) { d.Recovery.Journal.Committed = false })
		got := jvProblems(rfVerify(t, jc.s.Case))
		if len(got) != 1 || !strings.Contains(got[0], "committed") {
			t.Fatalf("journal problems %q", got)
		}
	})
}
