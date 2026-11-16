package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// The kernel-written F2FS roll-forward crash image (internal/filesys/f2fs/testdata/rollforward/) end to end through
// the real driver registry, against its independent oracle (tools/fixtures/f2fs_recover_oracle.py, subcommand
// rollforward). Every figure compared is read from the oracle file; none is restated here. The CLI is exercised by
// the command tests; this file covers the same run at the examine level (recover, journal report, block copies,
// case verify with every composed check, tampering).

type rfE2EOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Chain struct {
		Stop     string `json:"stop"`
		Complete bool   `json:"complete"`
		Nodes    []struct {
			Block uint32 `json:"block"`
		} `json:"nodes"`
	} `json:"chain"`
	Files []rfE2EFile `json:"files"`
}

type rfE2EFile struct {
	Path           string     `json:"path"`
	Family         string     `json:"family"`
	Ino            uint32     `json:"ino"`
	Kind           string     `json:"kind"`
	Size           int64      `json:"size"`
	ReplayedSHA256 string     `json:"replayed_sha256"`
	WrittenSHA256  string     `json:"written_sha256"`
	Expect         rfE2EWant  `json:"expect"`
	MapBlocks      [][2]int64 `json:"map_blocks"`
}

type rfE2EWant struct {
	Outcome        string     `json:"outcome"`
	Skip           string     `json:"skip"`
	Captured       [][2]int64 `json:"captured"`
	CapturedSHA256 string     `json:"captured_sha256"`
	Prefix         bool       `json:"prefix"`
	Cut            bool       `json:"cut"`
	Stop           string     `json:"stop"`
	EqualSource    bool       `json:"equal_source"`
	Confidence     int        `json:"confidence"`
}

func loadRFE2E(t *testing.T) ([]byte, rfE2EOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "f2fs", "testdata", "rollforward", "f2fs-rollforward")
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
	var o rfE2EOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatal("image sha256 does not match the oracle: regenerate image and oracle together")
	}
	return img, o
}

func rfE2ESHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// rfE2ERecords are the post-checkpoint-file artifacts of a run, by file id.
func rfE2ERecords(sum examine.RecoverSummary) map[string][]evidence.ManifestRecord {
	out := map[string][]evidence.ManifestRecord{}
	for _, a := range rwRecords(sum) {
		id := a.Source.Derived.FSID
		out[id] = append(out[id], a)
	}
	return out
}

func TestRollForwardFixtureOutcomes(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	img, o := loadRFE2E(t)
	s, disk := imageSession(t, img)
	sum := rwAll(t, s)
	got := rfE2ERecords(sum)

	if sum.Chain == nil || sum.Chain.Nodes != len(o.Chain.Nodes) || sum.Chain.Stop != o.Chain.Stop || sum.Chain.Complete != o.Chain.Complete {
		t.Errorf("run chain %+v, oracle %d nodes stop %q complete %v", sum.Chain, len(o.Chain.Nodes), o.Chain.Stop, o.Chain.Complete)
	}
	var wantRecovered, wantPartial int
	wantSkip := map[string]int{}
	explained := map[string]bool{}
	for _, f := range o.Files {
		x := f.Expect
		id := fmt.Sprintf("rollforward:%d", f.Ino)
		arts := got[id]
		switch x.Outcome {
		case "report-only":
			if len(arts) != 0 {
				t.Errorf("%s (%s): %d artifacts for a report-only file", f.Path, f.Kind, len(arts))
			}
			if f.Kind != "absent" {
				explained[id] = true
			}
			continue
		case "skip":
			explained[id] = true
			wantSkip[x.Skip]++
			if len(arts) != 0 {
				t.Errorf("%s: %d artifacts for a skipped file (%s)", f.Path, len(arts), x.Skip)
			}
			continue
		case "recover":
		default:
			t.Errorf("%s: unknown oracle outcome %q", f.Path, x.Outcome)
			continue
		}
		explained[id] = true
		wantRecovered++
		if x.Cut || x.Prefix {
			wantPartial++
		}
		if len(arts) != 1 {
			t.Errorf("%s: %d artifacts, want exactly 1", f.Path, len(arts))
			continue
		}
		rec := arts[0]
		rv := recoveryOf(rec)
		wantRuns := f2fsBlockRuns(x.Captured, f.Size)
		var wantBytes int64
		for _, r := range wantRuns {
			wantBytes += r.Length
		}
		data := readArtifact(t, s.Case, rec)
		if rec.Size != wantBytes || int64(len(data)) != wantBytes {
			t.Errorf("%s: artifact %d bytes, want the %d captured bytes", f.Path, rec.Size, wantBytes)
		}
		if gotRuns := mergeEvidenceRuns(rec.Source.Derived.Runs); !slices.Equal(gotRuns, mergeEvidenceRuns(wantRuns)) {
			t.Errorf("%s: runs %v, want the oracle's captured blocks %v", f.Path, gotRuns, wantRuns)
		}
		digest := rfE2ESHA(data)
		if x.CapturedSHA256 != "" && digest != x.CapturedSHA256 {
			t.Errorf("%s: artifact sha256 %s, oracle captured_sha256 %s", f.Path, digest, x.CapturedSHA256)
		}
		if equal := digest == f.WrittenSHA256; equal != x.EqualSource {
			t.Errorf("%s: artifact equals the written content: %v, the oracle says %v", f.Path, equal, x.EqualSource)
		}
		if x.EqualSource && f.ReplayedSHA256 != "" && digest != f.ReplayedSHA256 {
			t.Errorf("%s: artifact sha256 %s differs from the kernel replay's %s", f.Path, digest, f.ReplayedSHA256)
		}
		if rec.Incomplete != (x.Cut || x.Prefix) {
			t.Errorf("%s: Incomplete %v, the oracle says cut %v prefix %v", f.Path, rec.Incomplete, x.Cut, x.Prefix)
		}
		if rv.Class != evidence.ClassPostCheckpointFile || rv.Method != evidence.RollForwardMethod || rv.Confidence == nil || *rv.Confidence != x.Confidence {
			t.Errorf("%s: class %q method %q confidence %v; the oracle says %d", f.Path, rv.Class, rv.Method, ptrVal(rv.Confidence), x.Confidence)
		}
		if rv.Alloc.Free != wantBytes || rv.Alloc.Allocated != 0 || rv.Alloc.Unknown != 0 {
			t.Errorf("%s: alloc %+v, want %d free", f.Path, rv.Alloc, wantBytes)
		}
		if x.Stop != "" {
			if want := "stale-chain-stop=" + x.Stop; !hasAssumption(rv, want) {
				t.Errorf("%s: assumptions %v lack %q", f.Path, rv.Assumptions, want)
			}
		} else if hasAssumptionPrefix(rv, "stale-chain-stop=") {
			t.Errorf("%s: assumptions %v name a stop the oracle does not", f.Path, rv.Assumptions)
		}
		if !hasAssumption(rv, "not-checkpointed") || !hasAssumption(rv, "replay-not-applied") {
			t.Errorf("%s: assumptions %v lack the roll-forward caveats", f.Path, rv.Assumptions)
		}
		if rec.Source.Derived.FSPath != f.Path {
			t.Errorf("%s: FSPath %q", f.Path, rec.Source.Derived.FSPath)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	for id := range got {
		if !explained[id] {
			t.Errorf("artifact for %s: no oracle record asks for it", id) // logged and asserted 0
		}
	}
	if sum.Recovered != wantRecovered || sum.Partial != wantPartial || sum.Overlap != 0 || sum.Uniform != 0 {
		t.Errorf("Recovered/Partial/Overlap/Uniform = %d/%d/%d/%d, the oracle says %d/%d/0/0",
			sum.Recovered, sum.Partial, sum.Overlap, sum.Uniform, wantRecovered, wantPartial)
	}
	for reason, n := range wantSkip {
		if sum.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle says %d", reason, sum.SkippedBy[reason], n)
		}
	}
	for reason, n := range sum.SkippedBy {
		if wantSkip[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle expects %d", reason, n, wantSkip[reason])
		}
	}
	verifyOK(t, s.Case)
}

// References to files the oracle says are not extractable: the chain is complete, so absence is decided
// (ErrNotFound); an incomplete chain would give ErrIncomplete (builder tests: TestRecoverRollForwardMissingRefOnCutChainIsIncomplete).
func TestRollForwardFixtureRefsOnACompleteChain(t *testing.T) {
	if testing.Short() {
		t.Skip("imports a real-image fixture")
	}
	img, o := loadRFE2E(t)
	if !o.Chain.Complete {
		t.Fatal("the oracle chain is not complete; this test needs a complete one")
	}
	s, _ := imageSession(t, img)
	refs := []string{"id:rollforward:999999"}
	for _, f := range o.Files {
		if f.Kind == "live-update" {
			refs = append(refs, fmt.Sprintf("id:rollforward:%d", f.Ino))
		}
	}
	for _, ref := range refs {
		_, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, RollForward: true, Refs: []string{ref}})
		if !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
			t.Errorf("%s: %v, want ErrNotFound", ref, err)
		}
	}
}

// rfE2ECase runs the report, the block copies and the recovery on a fresh case of the fixture.
type rfE2ECase struct {
	s      *examine.Session
	o      rfE2EOracle
	report evidence.ManifestRecord
	blocks []evidence.ManifestRecord
	files  []evidence.ManifestRecord
}

func newRFE2ECase(t *testing.T) *rfE2ECase {
	t.Helper()
	img, o := loadRFE2E(t)
	s, _ := imageSession(t, img)
	c := &rfE2ECase{s: s, o: o}
	_, c.report = jrnReport(t, s, 0)
	bs := jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: jrnSeq(t, s)})
	if bs.Copied+bs.NotFree != len(o.Chain.Nodes) {
		t.Fatalf("block copies: %d copied, %d not free, the chain has %d nodes", bs.Copied, bs.NotFree, len(o.Chain.Nodes))
	}
	c.blocks = bs.Artifacts
	c.files = rwRecords(rwAll(t, s))
	return c
}

func TestRollForwardFixtureVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	c := newRFE2ECase(t)
	if len(c.blocks) == 0 || len(c.files) == 0 {
		t.Fatalf("%d block copies, %d recovered files", len(c.blocks), len(c.files))
	}
	rep := rfVerify(t, c.s.Case)
	if !rep.OK() {
		t.Fatalf("case verify problems: %v", rep.Problems)
	}
	// R12 (and with it R6 and R9) ran on every roll-forward artifact: nothing was only noticed
	if want := 1 + len(c.blocks) + len(c.files); rep.RollForwardRechecked != want {
		t.Errorf("RollForwardRechecked = %d, want %d (report + %d block copies + %d files)", rep.RollForwardRechecked, want, len(c.blocks), len(c.files))
	}
	if rep.AllocationRechecked < len(c.files) {
		t.Errorf("AllocationRechecked = %d for %d post-checkpoint artifacts", rep.AllocationRechecked, len(c.files))
	}
	if rep.RecoveredReproduced < len(c.files)+len(c.blocks) {
		t.Errorf("RecoveredReproduced = %d for %d recovered files and %d block copies", rep.RecoveredReproduced, len(c.files), len(c.blocks))
	}
	for _, n := range rep.Notices {
		if strings.Contains(n, `kind "report"`) {
			t.Errorf("the report was only noticed, not rendered again: %s", n)
		}
	}
}

func TestRollForwardFixtureTamperIsAProblem(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	for _, which := range []string{"post-checkpoint-artifact", "block-copy"} {
		t.Run(which, func(t *testing.T) {
			c := newRFE2ECase(t)
			target := c.files[0]
			if which == "block-copy" {
				target = c.blocks[0]
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
