package examine_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// The oracle of the kernel-written F2FS recovery fixtures (internal/filesys/f2fs/testdata/recover/), produced by
// tools/fixtures/f2fs_recover_oracle.py from the finished images alone: never Minutiae. Block ranges are inclusive
// absolute block numbers of the 4 KiB filesystem.
type f2fsRecOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	DeletedEntryCount int `json:"deleted_entry_count"`
	Deleted           []struct {
		Path       string     `json:"path"`
		Family     string     `json:"family"`
		Listed     bool       `json:"listed"`
		DirNID     uint32     `json:"dir_nid"`
		Ino        uint32     `json:"ino"`
		Size       int64      `json:"size"`
		SHA256     string     `json:"sha256"`
		TrueBlocks [][2]int64 `json:"true_blocks"`
		Stale      struct {
			Addr uint32 `json:"addr"`
		} `json:"stale"`
		Expect struct {
			Outcome     string     `json:"outcome"`
			Skip        string     `json:"skip"`
			Method      string     `json:"method"`
			Captured    [][2]int64 `json:"captured"`
			Cut         bool       `json:"cut"`
			Prefix      bool       `json:"prefix"`
			Stop        string     `json:"stop"`
			EqualSource bool       `json:"equal_source"`
			Uniform     bool       `json:"uniform"`
			Confidence  int        `json:"confidence"`
		} `json:"expect"`
	} `json:"deleted"`
}

func loadF2FSRecover(t *testing.T, name string) ([]byte, f2fsRecOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "f2fs", "testdata", "recover", name)
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
	var o f2fsRecOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

type f2fsKey struct {
	dir, ino uint32
	name     string
}

// f2fsDeletedIDs maps (directory nid, inode, name) of every deleted entry the reader lists to its entry id, by
// walking the raw image with the reader (the oracle has no ids). The count is the number of entries listed.
func f2fsDeletedIDs(t *testing.T, img []byte) (map[f2fsKey]string, int) {
	t.Helper()
	fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[f2fsKey]string{}
	n := 0
	if err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil || !e.Deleted {
			return nil
		}
		n++
		rest, _ := strings.CutPrefix(e.ID, "dentry:")
		dir, _ := strconv.ParseUint(strings.SplitN(rest, ":", 2)[0], 10, 32)
		var ino uint64
		for _, kv := range e.Attrs {
			if kv.Key == "inode" {
				ino, _ = strconv.ParseUint(kv.Value, 10, 32)
			}
		}
		ids[f2fsKey{uint32(dir), uint32(ino), path.Base(p)}] = e.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ids, n
}

// f2fsBlockRuns converts inclusive block ranges into image byte runs (the partition offset added), the total cut
// at size.
func f2fsBlockRuns(ranges [][2]int64, size int64) []evidence.Run {
	var runs []evidence.Run
	left := size
	for _, r := range ranges {
		length := min((r[1]-r[0]+1)*4096, left)
		if length <= 0 {
			break
		}
		runs = append(runs, evidence.Run{Offset: partStart + r[0]*4096, Length: length})
		left -= length
	}
	return runs
}

// mergeEvidenceRuns joins adjacent runs, so a map compares by the bytes it names and not by how it was cut.
func mergeEvidenceRuns(runs []evidence.Run) []evidence.Run {
	var out []evidence.Run
	for _, r := range runs {
		if n := len(out); n > 0 && out[n-1].Offset+out[n-1].Length == r.Offset {
			out[n-1].Length += r.Length
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestF2FSRecoverFixtureOutcomes recovers both kernel-written images through the real driver registry and
// compares every artifact with the independent oracle: bytes (image-relative runs), method, confidence, the
// incomplete flag, equality with the source, the stale-chain stop, overlap, the skip reasons, and case verify.
// The discard image is all not-found (probe gate G5): the kernel discarded the stale inode blocks, so nothing is
// recovered there and nothing uniform is written.
func TestF2FSRecoverFixtureOutcomes(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	for _, name := range []string{"f2fs-recover-nodiscard", "f2fs-recover-discard"} {
		t.Run(name, func(t *testing.T) { checkF2FSRecoverFixture(t, name) })
	}
}

func checkF2FSRecoverFixture(t *testing.T, name string) {
	img, o := loadF2FSRecover(t, name)
	ids, nListed := f2fsDeletedIDs(t, img)
	if nListed != o.DeletedEntryCount {
		t.Errorf("the reader lists %d deleted entries, the oracle counts %d", nListed, o.DeletedEntryCount)
	}
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)

	var wantRecovered, wantPartial, wantOverlap, wantUniform, explained int
	wantSkip := map[string]int{}
	seenID := map[string]bool{}
	for _, d := range o.Deleted {
		if !d.Listed {
			continue
		}
		id, ok := ids[f2fsKey{d.DirNID, d.Ino, path.Base(d.Path)}]
		if !ok {
			t.Errorf("%s: the reader lists no deleted entry with directory nid %d, inode %d", d.Path, d.DirNID, d.Ino)
			continue
		}
		explained++
		seenID[id] = true
		var got []evidence.ManifestRecord
		for _, a := range sum.Artifacts {
			if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID == id {
				got = append(got, a)
			}
		}
		x := d.Expect
		if x.Outcome == "skip" {
			wantSkip[x.Skip]++
			if len(got) != 0 {
				t.Errorf("%s: %d artifacts for a skipped entry (%s)", d.Path, len(got), x.Skip)
			}
			continue
		}
		wantRecovered++
		if x.Cut || x.Prefix {
			wantPartial++
		}
		if x.Confidence == 30 {
			wantOverlap++
		}
		if x.Uniform {
			wantUniform++
		}
		if len(got) != 1 {
			t.Errorf("%s: %d artifacts, want exactly 1", d.Path, len(got))
			continue
		}
		rec := got[0]
		rv := recoveryOf(rec)
		wantRuns := f2fsBlockRuns(x.Captured, d.Size)
		var wantBytes int64
		for _, r := range wantRuns {
			wantBytes += r.Length
		}
		data := readArtifact(t, s.Case, rec)
		if rec.Size != wantBytes || int64(len(data)) != wantBytes {
			t.Errorf("%s: artifact %d bytes, want the %d captured bytes", d.Path, rec.Size, wantBytes)
		}
		if gotRuns := mergeEvidenceRuns(rec.Source.Derived.Runs); !slices.Equal(gotRuns, mergeEvidenceRuns(wantRuns)) {
			t.Errorf("%s: runs %v, want the oracle's captured blocks %v", d.Path, gotRuns, wantRuns)
		}
		digest := sha256.Sum256(data)
		if equal := hex.EncodeToString(digest[:]) == d.SHA256; equal != x.EqualSource {
			t.Errorf("%s: artifact equals its source: %v, the oracle says %v (a not-equal recovery must never be)", d.Path, equal, x.EqualSource)
		}
		if rec.Incomplete != (x.Cut || x.Prefix) {
			t.Errorf("%s: Incomplete %v, the oracle says cut %v prefix %v", d.Path, rec.Incomplete, x.Cut, x.Prefix)
		}
		if rv.Class != evidence.ClassDeletedFile || rv.Method != x.Method || rv.Confidence == nil || *rv.Confidence != x.Confidence {
			t.Errorf("%s: class %q method %q confidence %v; the oracle says %s %d", d.Path, rv.Class, rv.Method, ptrVal(rv.Confidence), x.Method, x.Confidence)
		}
		if rv.Alloc.Free != wantBytes || rv.Alloc.Allocated != 0 || rv.Alloc.Unknown != 0 {
			t.Errorf("%s: alloc %+v, want %d free", d.Path, rv.Alloc, wantBytes)
		}
		if x.Stop != "" {
			if want := "stale-chain-stop=" + x.Stop; !hasAssumption(rv, want) {
				t.Errorf("%s: assumptions %v lack %q", d.Path, rv.Assumptions, want)
			}
			if !strings.Contains(rec.Error, "stale-chain-stop="+x.Stop) {
				t.Errorf("%s: artifact error %q does not name the stop", d.Path, rec.Error)
			}
		} else if hasAssumptionPrefix(rv, "stale-chain-stop=") {
			t.Errorf("%s: assumptions %v name a stop the oracle does not", d.Path, rv.Assumptions)
		}
		if want := fmt.Sprintf("node:nid=%d:blk=0x%x", d.Ino, d.Stale.Addr); !slices.Contains(rv.Basis, want) {
			t.Errorf("%s: basis %v lacks %q", d.Path, rv.Basis, want)
		}
		if (x.Confidence == 30) != hasAssumptionPrefix(rv, "overlap=") {
			t.Errorf("%s: confidence %d with assumptions %v", d.Path, x.Confidence, rv.Assumptions)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	if explained != nListed {
		t.Errorf("%d deleted entries listed by the reader are not explained by the oracle (never a silent cut)", nListed-explained)
	}
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID != "" && !seenID[a.Source.Derived.FSID] {
			t.Errorf("artifact for %s: no oracle record asks for it", a.Source.Derived.FSID)
		}
	}
	if sum.Recovered != wantRecovered || sum.Partial != wantPartial || sum.Overlap != wantOverlap || sum.Uniform != wantUniform {
		t.Errorf("Recovered/Partial/Overlap/Uniform = %d/%d/%d/%d, the oracle says %d/%d/%d/%d",
			sum.Recovered, sum.Partial, sum.Overlap, sum.Uniform, wantRecovered, wantPartial, wantOverlap, wantUniform)
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
	if sum.Considered != o.DeletedEntryCount {
		t.Errorf("Considered = %d, the oracle counts %d deleted entries", sum.Considered, o.DeletedEntryCount)
	}
	verifyOK(t, s.Case)
}

// ptrVal is the value behind p for a failure message, -1 for nil.
func ptrVal(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
