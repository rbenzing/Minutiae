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
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// The oracle of the kernel-written ext4 and ext3 recovery images (internal/filesys/ext4/testdata/recover/),
// produced by tools/fixtures/ext4_recover_oracle.py from the finished image and the stage images alone: never
// Minutiae. Block runs are [first block, count].
type e4Oracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	BlockSize         int64 `json:"block_size"`
	DeletedEntryCount int   `json:"deleted_entry_count"`
	Totals            struct {
		Recovered int            `json:"recovered"`
		SkippedBy map[string]int `json:"skipped_by"`
		Undecided int            `json:"undecided"`
	} `json:"totals"`
	Deleted []e4Deleted `json:"deleted"`
}

type e4Deleted struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Family       string `json:"family"`
	Inode        uint32 `json:"inode"`
	SourceSHA256 string `json:"source_sha256"`
	SourceSize   int64  `json:"source_size"`
	Expect       struct {
		Outcome        string     `json:"outcome"`
		Skip           string     `json:"skip"`
		Method         string     `json:"method"`
		Size           int64      `json:"size"`
		CapturedBlocks [][2]int64 `json:"captured_blocks"`
		Cut            bool       `json:"cut"`
		Incomplete     bool       `json:"incomplete"`
		Confidence     int        `json:"confidence"`
		Assumptions    []string   `json:"assumptions"`
		EqualSource    string     `json:"equal_source"`
	} `json:"expect"`
}

func loadE4Oracle(t testing.TB, name string) ([]byte, e4Oracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "ext4", "testdata", "recover", name)
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
	var o e4Oracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

// e4Key ties an oracle record to a reader entry: the name and the inode attribute (oracle inode 0 is "unknown").
func e4Key(name string, inode uint32) string {
	if inode == 0 {
		return name + "/unknown"
	}
	return name + "/" + strconv.FormatUint(uint64(inode), 10)
}

// e4DeletedIDs maps e4Key of every deleted entry the reader walks to its entry id (the oracle has no ids).
func e4DeletedIDs(t *testing.T, img []byte) map[string]string {
	t.Helper()
	fsys, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	if err := filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, werr error) error {
		if werr != nil || !e.Deleted {
			return nil
		}
		var inode string
		for _, kv := range e.Attrs {
			if kv.Key == "inode" {
				inode = kv.Value
			}
		}
		k := e.Name + "/" + inode
		if _, dup := ids[k]; dup {
			t.Fatalf("two deleted entries share the key %q", k)
		}
		ids[k] = e.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// e4ByteRuns turns [first block, count] runs into image byte runs (the partition offset added), merging runs that touch.
func e4ByteRuns(blocks [][2]int64, bs int64) []evidence.Run {
	var runs []evidence.Run
	for _, b := range blocks {
		runs = append(runs, evidence.Run{Offset: partStart + b[0]*bs, Length: b[1] * bs})
	}
	return e4MergeRuns(runs)
}

func e4MergeRuns(runs []evidence.Run) []evidence.Run {
	var out []evidence.Run
	for _, r := range runs {
		if n := len(out); n > 0 && out[n-1].Offset+out[n-1].Length == r.Offset {
			out[n-1].Length += r.Length
		} else {
			out = append(out, r)
		}
	}
	return out
}

// cutE4Runs keeps the first size bytes of runs.
func cutE4Runs(runs []evidence.Run, size int64) []evidence.Run {
	var out []evidence.Run
	for _, r := range runs {
		if size <= 0 {
			break
		}
		r.Length = min(r.Length, size)
		size -= r.Length
		out = append(out, r)
	}
	return out
}

func e4Sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// e4Relation is the relation of an artifact to the file the guest wrote. The source bytes are not in the repository,
// only their SHA-256 and size, so artifact-is-prefix is decided by length here and its bytes are held against the
// oracle's captured blocks by the caller.
func e4Relation(artifact []byte, d e4Deleted) string {
	switch {
	case int64(len(artifact)) == d.SourceSize && e4Sha(artifact) == d.SourceSHA256:
		return "full"
	case int64(len(artifact)) > d.SourceSize && e4Sha(artifact[:d.SourceSize]) == d.SourceSHA256:
		return "prefix-of-artifact"
	case int64(len(artifact)) < d.SourceSize:
		return "artifact-is-prefix"
	}
	return "no"
}

// Recovery of the whole kernel-written images through the real driver registry: every artifact equals the oracle
// (bytes, size, Incomplete as the oracle states it, method, confidence, assumptions), a recovery that is not equal to
// its source is never presented as equal, skips are counted under the oracle's reason, and case verify passes
// (R6 reproduced equals recovered, R9 clean). The wiped image recovers nothing.
func TestExt4RecoverMatchesOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range []string{"ext4-recover", "ext3-recover", "ext4-recover-sameboot", "ext4-recover-wiped"} {
		t.Run(name, func(t *testing.T) { checkE4Oracle(t, name) })
	}
}

func checkE4Oracle(t *testing.T, name string) {
	img, o := loadE4Oracle(t, name)
	ids := e4DeletedIDs(t, img)
	if len(ids) != o.DeletedEntryCount {
		t.Errorf("the reader walks %d deleted entries, the oracle says %d", len(ids), o.DeletedEntryCount)
	}
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	bs := o.BlockSize

	recoverIDs := map[string]bool{}
	var wantRecovered int
	for _, d := range o.Deleted {
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
		x := d.Expect
		if x.Outcome == "skip" {
			if len(got) != 0 {
				t.Errorf("%s: %d artifacts for a skipped entry (%s)", d.Path, len(got), x.Skip)
			}
			continue
		}
		recoverIDs[id] = true
		wantRecovered++
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
		if rel := e4Relation(data, d); rel != x.EqualSource {
			t.Errorf("%s: the artifact relates to its source as %q, the oracle says %q", d.Path, rel, x.EqualSource)
		}
		// Incomplete is the oracle's own field: it is set by a cut AND by file-end-unknown or a short map, so it is not
		// the same thing as Cut.
		if rec.Incomplete != x.Incomplete {
			t.Errorf("%s: Incomplete %v, the oracle says %v (cut %v)", d.Path, rec.Incomplete, x.Incomplete, x.Cut)
		}
		if rv.Class != evidence.ClassDeletedFile || rv.Method != x.Method || rv.Confidence == nil || *rv.Confidence != x.Confidence {
			t.Errorf("%s: class %q method %q confidence %v; the oracle says %s %d", d.Path, rv.Class, rv.Method, rv.Confidence, x.Method, x.Confidence)
		}
		for _, a := range x.Assumptions {
			if !hasAssumption(rv, a) {
				t.Errorf("%s: oracle assumption %q missing from %v", d.Path, a, rv.Assumptions)
			}
		}
		if rv.Alloc.Free != x.Size || rv.Alloc.Allocated != 0 || rv.Alloc.Unknown != 0 {
			t.Errorf("%s: alloc %+v, want %d free", d.Path, rv.Alloc, x.Size)
		}
		if gotRuns, wantRuns := e4MergeRuns(rec.Source.Derived.Runs), cutE4Runs(e4ByteRuns(x.CapturedBlocks, bs), x.Size); !slices.Equal(gotRuns, wantRuns) {
			t.Errorf("%s: runs %v, the oracle says %v", d.Path, gotRuns, wantRuns)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID != "" && !recoverIDs[a.Source.Derived.FSID] {
			t.Errorf("artifact for %s: no oracle record asks for it", a.Source.Derived.FSID)
		}
	}
	if sum.Recovered != o.Totals.Recovered || sum.Recovered != wantRecovered {
		t.Errorf("Recovered %d, the oracle says %d (%d records)", sum.Recovered, o.Totals.Recovered, wantRecovered)
	}
	for reason, n := range o.Totals.SkippedBy {
		if sum.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle says %d", reason, sum.SkippedBy[reason], n)
		}
	}
	for reason, n := range sum.SkippedBy {
		if o.Totals.SkippedBy[reason] != n {
			t.Errorf("SkippedBy[%s] = %d, the oracle lists %d", reason, n, o.Totals.SkippedBy[reason])
		}
	}
	if sum.SkippedBy["undecided"] != o.Totals.Undecided {
		t.Errorf("undecided = %d, the oracle says %d", sum.SkippedBy["undecided"], o.Totals.Undecided)
	}
	if o.DeletedEntryCount == 0 && (len(sum.Artifacts) != 0 || sum.Recovered != 0 || sum.Considered != 0) {
		t.Errorf("a wiped image recovers nothing, got %d artifacts, Recovered %d, Considered %d", len(sum.Artifacts), sum.Recovered, sum.Considered)
	}
	verifyOK(t, s.Case)
}
