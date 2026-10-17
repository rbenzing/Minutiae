package examine_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
)

// The oracle of the kernel-written recovery fixtures (internal/filesys/{fat,exfat}/testdata/recover/), produced by
// tools/fixtures/*_recover_oracle.py from the finished image alone: never Minutiae.
type fixRecover struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	ClusterSize       int64        `json:"cluster_size"`
	DataStart         int64        `json:"data_start"`
	DeletedEntryCount int          `json:"deleted_entry_count"`
	Deleted           []fixDeleted `json:"deleted"`
}

type fixDeleted struct {
	Path            string  `json:"path"`
	Family          string  `json:"family"`
	Listed          bool    `json:"listed"`
	Size            int64   `json:"size"`
	SHA256          string  `json:"sha256"`
	FirstCluster    int64   `json:"first_cluster"`
	NameState       string  `json:"name_state"`
	ValidDataLength int64   `json:"valid_data_length"`
	Expect          fixWant `json:"expect"`
}

type fixWant struct {
	Outcome     string     `json:"outcome"`
	Skip        string     `json:"skip"`
	Method      string     `json:"method"`
	Range       [][2]int64 `json:"range"`
	Captured    [][2]int64 `json:"captured"`
	Cut         bool       `json:"cut"`
	EqualSource bool       `json:"equal_source"`
	Overlaps    []string   `json:"overlaps"`
	Confidence  int        `json:"confidence"`
	Assumptions []string   `json:"assumptions"`
}

func loadFixRecover(t *testing.T, dir, name string) ([]byte, fixRecover) {
	t.Helper()
	base := filepath.Join("..", "filesys", dir, "testdata", "recover", name)
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
	var o fixRecover
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

// fixtureKey ties an oracle record to a reader entry: the first cluster and, for a file, the size.
func fixtureKey(first, size int64, dir bool) string {
	if dir {
		size = 0
	}
	return strconv.FormatInt(first, 10) + "/" + strconv.FormatInt(size, 10) + "/" + strconv.FormatBool(dir)
}

// fixtureIDs maps fixtureKey of every deleted entry to its entry id, by walking the raw image with the reader
// (the oracle has no ids).
func fixtureIDs(t *testing.T, dir string, img []byte) map[string]string {
	t.Helper()
	var fsys filesys.FileSystem
	var err error
	if dir == "fat" {
		fsys, err = fat.Open(bytes.NewReader(img), int64(len(img)))
	} else {
		fsys, err = exfat.Open(bytes.NewReader(img), int64(len(img)))
	}
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	if err := filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, werr error) error {
		if werr != nil || !e.Deleted {
			return nil
		}
		var first int64
		for _, kv := range e.Attrs {
			if kv.Key == "first_cluster" {
				first, _ = strconv.ParseInt(kv.Value, 10, 64)
			}
		}
		size := e.Size
		if dir == "fat" {
			for _, kv := range e.Attrs {
				if kv.Key == "size" {
					size, _ = strconv.ParseInt(kv.Value, 10, 64)
				}
			}
		}
		ids[fixtureKey(first, size, e.Type == filesys.TypeDir)] = e.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// byteRuns converts the oracle's cluster ranges into image byte runs (the partition offset added), the total cut at size.
func byteRuns(o fixRecover, ranges [][2]int64, size int64) []evidence.Run {
	var runs []evidence.Run
	left := size
	for _, r := range ranges {
		length := min((r[1]-r[0]+1)*o.ClusterSize, left)
		if length <= 0 {
			break
		}
		runs = append(runs, evidence.Run{Offset: partStart + o.DataStart + (r[0]-2)*o.ClusterSize, Length: length})
		left -= length
	}
	return runs
}

// skipFirst drops the first n bytes from runs.
func skipFirst(runs []evidence.Run, n int64) []evidence.Run {
	var out []evidence.Run
	for _, r := range runs {
		switch {
		case n >= r.Length:
			n -= r.Length
		case n > 0:
			out = append(out, evidence.Run{Offset: r.Offset + n, Length: r.Length - n})
			n = 0
		default:
			out = append(out, r)
		}
	}
	return out
}

func runBytes(runs []evidence.Run) (n int64) {
	for _, r := range runs {
		n += r.Length
	}
	return n
}

// Recovery of the whole kernel-written images through the real driver registry: every artifact equals the
// oracle (bytes, method, confidence, alloc state, exclusions), a not-equal recovery is never presented as equal to
// its source, skips are counted under the oracle's reason, nothing is recovered for a listed:false record, and
// case verify passes.
func TestFATRecoverFixtureOutcomes(t *testing.T) {
	for _, name := range []string{"fat12-recover", "fat16-recover", "fat32-recover"} {
		t.Run(name, func(t *testing.T) { checkRecoverFixture(t, "fat", name) })
	}
}

func TestExfatRecoverFixtureOutcomes(t *testing.T) {
	checkRecoverFixture(t, "exfat", "exfat-recover")
}

func checkRecoverFixture(t *testing.T, dir, name string) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	img, o := loadFixRecover(t, dir, name)
	ids := fixtureIDs(t, dir, img)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)

	var wantRecovered, wantPartial, wantOverlap int
	wantSkip := map[string]int{}
	recoverIDs := map[string]bool{}
	for _, d := range o.Deleted {
		if !d.Listed {
			continue
		}
		size := d.Size
		if dir == "exfat" {
			size = d.ValidDataLength
		}
		id, ok := ids[fixtureKey(d.FirstCluster, d.Size, d.Expect.Skip == "not-a-file")]
		if !ok {
			t.Errorf("%s: the reader lists no deleted entry with first cluster %d", d.Path, d.FirstCluster)
			continue
		}
		var got []evidence.ManifestRecord
		for _, a := range sum.Artifacts {
			if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID == id {
				got = append(got, a)
			}
		}
		if d.Expect.Outcome == "skip" {
			wantSkip[d.Expect.Skip]++
			if len(got) != 0 {
				t.Errorf("%s: %d artifacts for a skipped entry (%s)", d.Path, len(got), d.Expect.Skip)
			}
			continue
		}
		recoverIDs[id] = true
		wantRecovered++
		if d.Expect.Cut {
			wantPartial++
		}
		if len(d.Expect.Overlaps) > 0 {
			wantOverlap++
		}
		if len(got) != 1 {
			t.Errorf("%s: %d artifacts, want exactly 1", d.Path, len(got))
			continue
		}
		rec := got[0]
		rv := recoveryOf(rec)
		capBytes := min(runBytes(byteRuns(o, d.Expect.Captured, size)), size)
		data := readArtifact(t, s.Case, rec)
		if rec.Size != capBytes || int64(len(data)) != capBytes {
			t.Errorf("%s: artifact %d bytes, want the %d captured bytes", d.Path, rec.Size, capBytes)
		}
		digest := sha256.Sum256(data)
		if equal := hex.EncodeToString(digest[:]) == d.SHA256; equal != d.Expect.EqualSource {
			t.Errorf("%s: artifact equals its source: %v, the oracle says %v (a not-equal recovery must never be)", d.Path, equal, d.Expect.EqualSource)
		}
		if rec.Incomplete != d.Expect.Cut {
			t.Errorf("%s: Incomplete %v, the oracle says cut %v", d.Path, rec.Incomplete, d.Expect.Cut)
		}
		if rv.Class != evidence.ClassDeletedFile || rv.Method != d.Expect.Method || rv.Confidence == nil || *rv.Confidence != d.Expect.Confidence {
			t.Errorf("%s: class %q method %q confidence %v; the oracle says %s %d", d.Path, rv.Class, rv.Method, rv.Confidence, d.Expect.Method, d.Expect.Confidence)
		}
		if rv.Alloc.Free != capBytes || rv.Alloc.Allocated != 0 || rv.Alloc.Unknown != 0 {
			t.Errorf("%s: alloc %+v, want %d free", d.Path, rv.Alloc, capBytes)
		}
		wantExcl := skipFirst(byteRuns(o, d.Expect.Range, size), capBytes)
		if !slices.Equal(rv.Excluded, wantExcl) && (len(rv.Excluded) != 0 || len(wantExcl) != 0) {
			t.Errorf("%s: excluded %v, want range minus captured %v", d.Path, rv.Excluded, wantExcl)
		}
		if len(d.Expect.Overlaps) > 0 && !hasAssumptionPrefix(rv, "overlap=") {
			t.Errorf("%s: overlaps %v but assumptions %v name none", d.Path, d.Expect.Overlaps, rv.Assumptions)
		}
		for _, a := range d.Expect.Assumptions { // F110: the oracle words its tokens its own way; a fixed table maps each to the reader token
			want, ok := exfattest.ReaderAssumptionFor(a)
			if !ok {
				t.Errorf("%s: the oracle token %q is not in the mapping table", d.Path, a)
			} else if !hasAssumption(rv, want) {
				t.Errorf("%s: oracle token %q requires %q, assumptions are %v", d.Path, a, want, rv.Assumptions)
			}
		}
		hasDirent, hasFirst := false, false
		for _, b := range rv.Basis {
			hasDirent = hasDirent || strings.HasPrefix(b, "dirent:")
			hasFirst = hasFirst || b == "first_cluster="+strconv.FormatInt(d.FirstCluster, 10)
		}
		if !hasDirent || !hasFirst {
			t.Errorf("%s: basis %v lacks dirent: or first_cluster=", d.Path, rv.Basis)
		}
		if dir == "fat" {
			basis := func(m string) bool { return slices.Contains(rv.Basis, m) }
			switch d.NameState {
			case "lfn-intact":
				if !basis("lfn=recovered") || basis("lfn_tail=unterminated") || basis("name=short-only") {
					t.Errorf("%s: name_state lfn-intact but basis %v", d.Path, rv.Basis)
				}
			case "lfn-tail-lost":
				if !basis("lfn=recovered") || !basis("lfn_tail=unterminated") {
					t.Errorf("%s: name_state lfn-tail-lost but basis %v", d.Path, rv.Basis)
				}
			case "short-only":
				if !basis("name=short-only") || basis("lfn=recovered") || basis("lfn_tail=unterminated") {
					t.Errorf("%s: name_state short-only but basis %v", d.Path, rv.Basis)
				}
			}
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID != "" && !recoverIDs[a.Source.Derived.FSID] {
			t.Errorf("artifact for %s: no oracle record asks for it", a.Source.Derived.FSID)
		}
	}
	if sum.Recovered != wantRecovered || sum.Partial != wantPartial || sum.Overlap != wantOverlap {
		t.Errorf("Recovered/Partial/Overlap = %d/%d/%d, the oracle says %d/%d/%d", sum.Recovered, sum.Partial, sum.Overlap, wantRecovered, wantPartial, wantOverlap)
	}
	for reason, n := range wantSkip {
		if sum.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle says %d", reason, sum.SkippedBy[reason], n)
		}
	}
	verifyOK(t, s.Case)
}
