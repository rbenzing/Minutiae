package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// The kernel-written ext4 and ext3 journal images (internal/filesys/ext4/testdata/journal/, plan 3G Task 14) end to end through
// the real driver registry, against their independent oracle (tools/fixtures/ext4_journal_oracle.py: never Minutiae). Every figure
// compared is read from the oracle file; the only values written here are the per-family intentions of jeFamilyWant, which hold
// the oracle itself to the plan's table. Skipped under -short like the other fixture end-to-end tests.

type jeExpect struct {
	Outcome        string     `json:"outcome"`
	Skip           string     `json:"skip"`
	Method         string     `json:"method"`
	Seq            uint32     `json:"seq"`
	Region         string     `json:"region"`
	Size           int64      `json:"size"`
	CapturedBlocks [][2]int64 `json:"captured_blocks"`
	Cut            bool       `json:"cut"`
	Incomplete     bool       `json:"incomplete"`
	Confidence     int        `json:"confidence"`
	Assumptions    []string   `json:"assumptions"`
	EqualSource    string     `json:"equal_source"`
}

type jeDeleted struct {
	Path         string              `json:"path"`
	Name         string              `json:"name"`
	Family       string              `json:"family"`
	Inode        uint32              `json:"inode"`
	SourceSHA256 string              `json:"source_sha256"`
	SourceSize   int64               `json:"source_size"`
	Expect       map[string]jeExpect `json:"expect"`
}

type jeTxn struct {
	Seq       uint32 `json:"seq"`
	Region    string `json:"region"`
	Committed bool   `json:"committed"`
	Tags      []struct {
		FSBlock int64  `json:"fsblock"`
		Copy    string `json:"copy"`
		Revoked bool   `json:"revoked"`
	} `json:"tags"`
}

type jeOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	BlockSize         int64 `json:"block_size"`
	DeletedEntryCount int   `json:"deleted_entry_count"`
	Totals            map[string]struct {
		Recovered int            `json:"recovered"`
		SkippedBy map[string]int `json:"skipped_by"`
		Undecided int            `json:"undecided"`
	} `json:"totals"`
	Journal struct {
		NeedsRecovery bool `json:"needs_recovery"`
	} `json:"journal"`
	Txns    []jeTxn     `json:"txns"`
	Deleted []jeDeleted `json:"deleted"`
}

var jeImages = []string{"ext4-journal-clean", "ext4-journal-crash", "ext3-journal-clean"}

type jeAnswer struct {
	skip, method, region string
	conf                 int
	equal                string
}

// jeFamilyWant is the plan's per-family table (tools/fixtures/README.md "Families") by journal mode; a family or mode not
// listed is held to the oracle only.
var jeFamilyWant = map[string]map[string]jeAnswer{
	"jgone":     {"none": {skip: "no-map"}, "committed": {skip: "no-map"}, "stale": {skip: "no-map"}},
	"jtrunc":    {"none": {skip: "no-map"}, "committed": {skip: "no-map"}, "stale": {skip: "no-map"}},
	"jhardlink": {"none": {skip: "reused"}, "committed": {skip: "reused"}, "stale": {skip: "reused"}},
	"jinline":   {"none": {skip: "unsupported"}, "committed": {skip: "unsupported"}, "stale": {skip: "unsupported"}},
	"jsmall":    {"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"}},
	"jdeep": {
		"committed": {method: "ext4-extent-leaf", conf: 20, equal: "prefix-of-artifact"},
		"stale":     {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"},
	},
	"jreuse": {"stale": {method: "ext4-journal-inode", region: "stale", conf: 20, equal: "artifact-is-prefix"}},
	"jswap":  {"stale": {method: "ext4-journal-inode", region: "stale", conf: 30, equal: "no"}},
	"crash-live-copy": {
		"none":      {skip: "no-map"},
		"committed": {method: "ext4-journal-inode", region: "live", conf: 40, equal: "full"},
		"stale":     {method: "ext4-journal-inode", region: "live", conf: 40, equal: "full"},
	},
	"bm-small":    {"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"}},
	"bm-indirect": {"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"}},
}

func loadJEOracle(t testing.TB, name string) ([]byte, jeOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "ext4", "testdata", "journal", name)
	gz, err := os.ReadFile(base + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o jeOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

func jeRecover(t *testing.T, s *examine.Session, mode string) examine.RecoverSummary {
	t.Helper()
	use := filesys.JournalUse(mode)
	if use == filesys.JournalUseNone {
		use = ""
	}
	sum, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: use})
	if err != nil {
		t.Fatalf("Recover(%q): %v", mode, err)
	}
	return sum
}

// TestExt4JournalRecoverMatchesOracle: for every journal mode, Recover(All) gives per record the oracle's outcome, captured
// blocks, Incomplete, confidence and relation to the source; the totals equal the oracle's; case verify holds with R6, R9 and
// R13 clean.
func TestExt4JournalRecoverMatchesOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jeImages {
		for _, mode := range []string{"none", "committed", "stale"} {
			t.Run(name+"/"+mode, func(t *testing.T) { checkJEOracle(t, name, mode) })
		}
	}
}

func checkJEOracle(t *testing.T, name, mode string) {
	img, o := loadJEOracle(t, name)
	ids := e4DeletedIDs(t, img)
	if len(ids) != o.DeletedEntryCount {
		t.Errorf("the reader walks %d deleted entries, the oracle says %d", len(ids), o.DeletedEntryCount)
	}
	s, disk := imageSession(t, img)
	sum := jeRecover(t, s, mode)
	bs := o.BlockSize
	tot := o.Totals[mode]

	asked := map[string]bool{}
	var wantRecovered, wantPartial, journalFiles int
	for _, d := range o.Deleted {
		if want, ok := jeFamilyWant[d.Family][mode]; ok {
			x := d.Expect[mode]
			switch {
			case want.skip != "":
				if x.Outcome != "skip" || x.Skip != want.skip {
					t.Errorf("%s (%s) %s: the oracle says %s %q, the plan's table says skip %q", d.Path, d.Family, mode, x.Outcome, x.Skip, want.skip)
				}
			case x.Outcome != "recover" || x.Method != want.method || x.Region != want.region || x.Confidence != want.conf || x.EqualSource != want.equal:
				t.Errorf("%s (%s) %s: the oracle says %s %s/%s conf %d equal %q, the plan's table says %s/%s conf %d equal %q",
					d.Path, d.Family, mode, x.Outcome, x.Method, x.Region, x.Confidence, x.EqualSource, want.method, want.region, want.conf, want.equal)
			}
		}
		id, ok := ids[e4Key(d.Name, d.Inode)]
		if !ok {
			t.Errorf("%s: the reader lists no deleted entry %s", d.Path, e4Key(d.Name, d.Inode))
			continue
		}
		var got []evidence.ManifestRecord
		for _, a := range sum.Artifacts {
			if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID == id {
				got = append(got, a)
			}
		}
		x := d.Expect[mode]
		if x.Outcome == "skip" {
			if len(got) != 0 {
				t.Errorf("%s: %d artifacts for a skipped entry (%s)", d.Path, len(got), x.Skip)
			}
			continue
		}
		asked[id] = true
		wantRecovered++
		if x.Incomplete {
			wantPartial++
		}
		if len(got) != 1 {
			t.Errorf("%s: %d artifacts, want exactly 1", d.Path, len(got))
			continue
		}
		rec := got[0]
		rv := recoveryOf(rec)
		data := readArtifact(t, s.Case, rec)
		if rec.Size != x.Size || int64(len(data)) != x.Size {
			t.Errorf("%s: artifact %d bytes, the oracle says %d", d.Path, rec.Size, x.Size)
		}
		var want []byte
		for _, r := range e4ByteRuns(x.CapturedBlocks, bs) {
			want = append(want, disk[r.Offset:r.Offset+r.Length]...)
		}
		if !bytes.Equal(data, want[:min(int64(len(want)), x.Size)]) {
			t.Errorf("%s: the artifact bytes are not the oracle's captured blocks cut at %d", d.Path, x.Size)
		}
		if rel := e4Relation(data, e4Deleted{SourceSHA256: d.SourceSHA256, SourceSize: d.SourceSize}); rel != x.EqualSource {
			t.Errorf("%s: the artifact relates to its source as %q, the oracle says %q", d.Path, rel, x.EqualSource)
		}
		if rec.Incomplete != x.Incomplete {
			t.Errorf("%s: Incomplete %v, the oracle says %v (cut %v)", d.Path, rec.Incomplete, x.Incomplete, x.Cut)
		}
		if rv.Class != evidence.ClassDeletedFile || rv.Method != x.Method || rv.Confidence == nil || *rv.Confidence != x.Confidence {
			t.Errorf("%s: class %q method %q confidence %v; the oracle says %s %d", d.Path, rv.Class, rv.Method, ptrVal(rv.Confidence), x.Method, x.Confidence)
		}
		overlaps := 0
		for _, a := range x.Assumptions {
			if other, ok := strings.CutPrefix(a, "overlap="); ok {
				// the oracle names the other entry by path, the recovery by entry id
				i := slices.IndexFunc(o.Deleted, func(e jeDeleted) bool { return e.Path == other })
				if i < 0 {
					t.Errorf("%s: the oracle's overlap %q is no record of the oracle", d.Path, other)
					continue
				}
				overlaps++
				a = "overlap=" + ids[e4Key(o.Deleted[i].Name, o.Deleted[i].Inode)]
			}
			if !hasAssumption(rv, a) {
				t.Errorf("%s: oracle assumption %q missing from %v", d.Path, a, rv.Assumptions)
			}
		}
		if got := countAssumptionPrefix(rv, "overlap="); got != overlaps {
			t.Errorf("%s: %d overlap assumptions, the oracle names %d: %v", d.Path, got, overlaps, rv.Assumptions)
		}
		if rv.Alloc.Free != x.Size || rv.Alloc.Allocated != 0 || rv.Alloc.Unknown != 0 {
			t.Errorf("%s: alloc %+v, want %d free", d.Path, rv.Alloc, x.Size)
		}
		if x.Method == "ext4-journal-inode" {
			journalFiles++
			if rv.Journal == nil || rv.Journal.Seq != x.Seq || rv.Journal.Region != x.Region || rv.Journal.Revoked || !rv.Journal.Committed {
				t.Errorf("%s: journal reference %+v, the oracle says seq %d region %s", d.Path, rv.Journal, x.Seq, x.Region)
			}
			if !hasAssumption(rv, "journal-not-replayed") && x.Region == "live" {
				t.Errorf("%s: a live-region copy lacks journal-not-replayed: %v", d.Path, rv.Assumptions)
			}
		} else if rv.Journal != nil {
			t.Errorf("%s: method %s carries a journal reference %+v", d.Path, x.Method, rv.Journal)
		}
		if gotRuns, wantRuns := e4MergeRuns(rec.Source.Derived.Runs), cutE4Runs(e4ByteRuns(x.CapturedBlocks, bs), x.Size); !slices.Equal(gotRuns, wantRuns) {
			t.Errorf("%s: runs %v, the oracle says %v", d.Path, gotRuns, wantRuns)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID != "" && !asked[a.Source.Derived.FSID] {
			t.Errorf("artifact for %s: no oracle record asks for it", a.Source.Derived.FSID)
		}
	}
	if sum.Recovered != tot.Recovered || sum.Recovered != wantRecovered {
		t.Errorf("Recovered %d, the oracle says %d (%d records)", sum.Recovered, tot.Recovered, wantRecovered)
	}
	if sum.Partial != wantPartial {
		t.Errorf("Partial %d, the oracle's incomplete records number %d", sum.Partial, wantPartial)
	}
	for reason, n := range tot.SkippedBy {
		if sum.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle says %d", reason, sum.SkippedBy[reason], n)
		}
	}
	for reason, n := range sum.SkippedBy {
		if tot.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle lists %d", reason, n, tot.SkippedBy[reason])
		}
	}
	if sum.SkippedBy["undecided"] != tot.Undecided {
		t.Errorf("undecided = %d, the oracle says %d", sum.SkippedBy["undecided"], tot.Undecided)
	}

	rep := rfVerify(t, s.Case)
	if !rep.OK() {
		t.Fatalf("case verify problems: %v", rep.Problems)
	}
	// R6 reproduced every recovered file; R13 recomputed the journal ones (R9 holds what the allocation check covers)
	if rep.RecoveredReproduced < wantRecovered {
		t.Errorf("RecoveredReproduced = %d for %d recovered files", rep.RecoveredReproduced, wantRecovered)
	}
	if rep.JournalRechecked != journalFiles {
		t.Errorf("JournalRechecked = %d, want %d (the ext4-journal-inode files)", rep.JournalRechecked, journalFiles)
	}
	if rep.AllocationRechecked < wantRecovered {
		t.Errorf("AllocationRechecked = %d for %d recovered files", rep.AllocationRechecked, wantRecovered)
	}
}

// jeCase is a case of one fixture holding the journal report, the block copies of one live and one stale transaction and the
// files recovered with --use-journal stale.
type jeCase struct {
	s      *examine.Session
	o      jeOracle
	report evidence.ManifestRecord
	blocks []evidence.ManifestRecord
	sums   []examine.JournalBlocksSummary
	seqs   []uint32
	files  []evidence.ManifestRecord
}

// jeSeqs picks, from the oracle, one live and one stale transaction whose sequence number names exactly one transaction
// and that logs at least one block the copy will accept; either may be absent (a clean image has no live one).
func jeSeqs(o jeOracle) (live, stale []jeTxn) {
	count := map[uint32]int{}
	for _, x := range o.Txns {
		count[x.Seq]++
	}
	for _, x := range o.Txns {
		if count[x.Seq] != 1 || !x.Committed {
			continue
		}
		ok := false
		for _, g := range x.Tags {
			ok = ok || (g.Copy == "verified" || g.Copy == "unverified") && !g.Revoked
		}
		if !ok {
			continue
		}
		if x.Region == "live" {
			live = append(live, x)
		} else {
			stale = append(stale, x)
		}
	}
	return live, stale
}

func newJECase(t *testing.T, name string) *jeCase {
	t.Helper()
	img, o := loadJEOracle(t, name)
	s, _ := imageSession(t, img)
	c := &jeCase{s: s, o: o}
	_, c.report = jbReport(t, s, jo, 0)
	live, stale := jeSeqs(o)
	var picks []jeTxn
	if len(live) > 0 {
		picks = append(picks, live[0])
	}
	if len(stale) > 0 {
		picks = append(picks, stale[len(stale)/2])
	}
	if len(picks) == 0 {
		t.Fatal("the oracle has no transaction to copy")
	}
	for _, p := range picks {
		sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: p.Seq, Region: p.Region})
		if err != nil || sum.Copied == 0 {
			t.Fatalf("JournalBlocks(seq %d %s): copied %d, %v", p.Seq, p.Region, sum.Copied, err)
		}
		c.blocks = append(c.blocks, sum.Artifacts...)
		c.sums = append(c.sums, sum)
		c.seqs = append(c.seqs, p.Seq)
	}
	c.files = jeJournalFiles(jeRecover(t, s, "stale"))
	return c
}

// jeJournalFiles are the recovered files whose map came from the journal.
func jeJournalFiles(sum examine.RecoverSummary) []evidence.ManifestRecord {
	var out []evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if rv := recoveryOfOrNil(a); a.Source.Kind == evidence.KindRecover && rv != nil && rv.Method == evidence.Ext4JournalMethod+"-inode" {
			out = append(out, a)
		}
	}
	return out
}

func TestExt4JournalFixtureVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jeImages {
		t.Run(name, func(t *testing.T) {
			c := newJECase(t, name)
			if len(c.files) == 0 {
				t.Fatal("no file recovered from the journal")
			}
			rep := rfVerify(t, c.s.Case)
			if !rep.OK() {
				t.Fatalf("case verify problems: %v", rep.Problems)
			}
			if want := 1 + len(c.blocks) + len(c.files); rep.JournalRechecked != want {
				t.Errorf("JournalRechecked = %d, want %d (report + %d block copies + %d files)", rep.JournalRechecked, want, len(c.blocks), len(c.files))
			}
			if rep.RollForwardRechecked != 0 {
				t.Errorf("RollForwardRechecked = %d for an ext4 case", rep.RollForwardRechecked)
			}
			if rep.RecoveredReproduced < len(c.files)+len(c.blocks) {
				t.Errorf("RecoveredReproduced = %d for %d files and %d block copies", rep.RecoveredReproduced, len(c.files), len(c.blocks))
			}
			for _, n := range rep.Notices {
				if strings.Contains(n, `kind "report"`) {
					t.Errorf("the report was only noticed, not rendered again: %s", n)
				}
			}
		})
	}
}

// 3G-R17 and C17: a copy taken where the allocation view is unknown (a filesystem that warns, as the crash image does) says
// so with the basis and assumption pair and is counted; where the view is known it carries neither. R13 holds both ways
// (TestVerifyJournalClean above recomputes them: the case verifies clean).
func TestExt4JournalFixtureBlockCopiesCarryTheC17Markers(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jeImages {
		t.Run(name, func(t *testing.T) {
			c := newJECase(t, name)
			fsys, _, err := c.s.FS(-1)
			if err != nil {
				t.Fatal(err)
			}
			warns := len(fsys.Info().Warnings) > 0
			if warns != c.o.Journal.NeedsRecovery {
				t.Fatalf("the filesystem warns: %v, the oracle's needs_recovery: %v (the crash image is the one that warns)", warns, c.o.Journal.NeedsRecovery)
			}
			for _, rec := range c.blocks {
				rv := recoveryOf(rec)
				marked := hasAssumption(rv, "bitmap-unconfirmed")
				if marked != warns || slices.Contains(rv.Basis, "alloc-basis=journal-inode") != warns {
					t.Errorf("%s: bitmap-unconfirmed %v, alloc-basis %v; the filesystem warns: %v", rec.Path, marked, slices.Contains(rv.Basis, "alloc-basis=journal-inode"), warns)
				}
			}
			for _, sum := range c.sums {
				if warns && sum.AllocUnknown != sum.Copied || !warns && sum.AllocUnknown != 0 {
					t.Errorf("AllocUnknown = %d for %d copies; the filesystem warns: %v", sum.AllocUnknown, sum.Copied, warns)
				}
			}
			if rep := rfVerify(t, c.s.Case); !rep.OK() {
				t.Errorf("case verify problems: %v", rep.Problems)
			}
		})
	}
}

func TestExt4JournalFixtureTamperIsAProblem(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, which := range []string{"journal-inode-artifact", "block-copy", "report"} {
		t.Run(which, func(t *testing.T) {
			c := newJECase(t, "ext4-journal-crash")
			target := c.files[0]
			switch which {
			case "block-copy":
				target = c.blocks[0]
			case "report":
				target = c.report
			}
			evidencetest.FlipArtifactByte(t, c.s.Case.Dir, target, target.Size/2)
			rep := rfVerify(t, c.s.Case)
			if rep.OK() {
				t.Fatalf("a flipped byte in %s %s verified", which, target.Path)
			}
			named := false
			for _, p := range rep.Problems {
				named = named || strings.Contains(p, target.Path) || strings.Contains(p, target.ID)
			}
			if !named {
				t.Errorf("no problem names the tampered artifact %s: %v", target.Path, rep.Problems)
			}
		})
	}
}

func countAssumptionPrefix(rv *evidence.Recovery, prefix string) int {
	n := 0
	for _, a := range rv.Assumptions {
		if strings.HasPrefix(a, prefix) {
			n++
		}
	}
	return n
}
