package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
)

// popOracle is the part of internal/filesys/apfs/testdata/apfs-populated.expect.json
// used here. The oracle (tools/fixtures/apfs_populated_oracle.py) decodes the
// image on its own and compares the result with the source tree that the
// linux-apfs-rw kernel driver, running under QEMU, was fed to write it.
type popOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Container struct {
		BlockSize int64 `json:"block_size"`
	} `json:"container"`
	Volume struct {
		Name string `json:"name"`
	} `json:"volume"`
	Live struct {
		Tree []popOracleNode `json:"tree"`
	} `json:"live"`
	Snapshots []struct {
		Name string          `json:"name"`
		Xid  uint64          `json:"xid"`
		Tree []popOracleNode `json:"tree"`
	} `json:"snapshots"`
	FreeRanges [][2]int64 `json:"free_ranges"` // inclusive block ranges
}

type popOracleNode struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MtimeNs int64  `json:"mtime_ns"`
	Sha256  string `json:"sha256"`
	Link    string `json:"link"`
}

func loadPopulatedAPFS(t *testing.T) ([]byte, popOracle) {
	t.Helper()
	dir := filepath.Join("..", "filesys", "apfs", "testdata")
	gz, err := os.ReadFile(filepath.Join(dir, "apfs-populated.img.gz"))
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
	raw, err := os.ReadFile(filepath.Join(dir, "apfs-populated.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o popOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s: regenerate the image and its expect.json together", got, o.Generator.ImageSHA256)
	}
	return img, o
}

// TestExtractFromPopulatedAPFS imports the real populated container (written
// by a real kernel driver) and extracts the live volume and snapshot paths:
// every artifact's size, SHA-256 and content equal the oracle's (which equals
// the source tree), the provenance carries the oracle's mode and modification
// time, the recorded runs reproduce the artifact from the image (fragmented,
// sparse and cloned files included), the unallocated export is exactly the
// oracle's free space, no filesystem warning is raised and case verify is OK.
func TestExtractFromPopulatedAPFS(t *testing.T) {
	if testing.Short() {
		t.Skip("extracts about 650 files and exports 25 MiB of free space")
	}
	img, want := loadPopulatedAPFS(t)
	c := newCase(t)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	vol := "/" + want.Volume.Name

	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{vol}, Recursive: true})
	got := extractedByPath(sum)
	wantFiles := 0
	for _, n := range want.Live.Tree {
		if n.Type == "dir" {
			continue
		}
		wantFiles++
		p := vol + n.Path
		rec, ok := got[p]
		if !ok {
			t.Errorf("no artifact for %s", p)
			continue
		}
		content := readArtifact(t, c, rec)
		size, sha := n.Size, n.Sha256
		if n.Type == "symlink" {
			// A symlink's artifact is its target.
			if string(content) != n.Link {
				t.Errorf("%s: artifact %q, oracle target %q", p, content, n.Link)
			}
			continue
		}
		if rec.Size != size || rec.SHA256 != sha || sha256hex(content) != sha || int64(len(content)) != size {
			t.Errorf("%s: artifact size/sha256 %d/%s, oracle %d/%s", p, rec.Size, rec.SHA256, size, sha)
		}
		d := rec.Source.Derived
		if d.ParentSHA256 != sha256hex(img) || d.FSType != "apfs" || rec.Incomplete || d.Snapshot != nil {
			t.Errorf("%s: derived %+v incomplete %v", p, d, rec.Incomplete)
		}
		if d.Mode&0o7777 != n.Mode {
			t.Errorf("%s: mode %04o, oracle %04o", p, d.Mode&0o7777, n.Mode)
		}
		if w := time.Unix(0, n.MtimeNs).UTC().Format(time.RFC3339Nano); d.Times["modified"] != w {
			t.Errorf("%s: modified %q, oracle %q", p, d.Times["modified"], w)
		}
		if size > 0 && !bytes.Equal(readImageRuns(img, d.Runs), content) {
			t.Errorf("%s: the recorded runs do not reproduce the artifact", p)
		}
	}
	if sum.Files != wantFiles || len(got) != wantFiles || sum.Skipped != 0 || sum.FSWarnings != 0 {
		t.Errorf("Files = %d, artifacts %d, Skipped = %d, FSWarnings = %d; want %d files and nothing skipped or warned", sum.Files, len(got), sum.Skipped, sum.FSWarnings, wantFiles)
	}
	// The fragmented and the sparse files are what the oracle says they are.
	if r := got[vol+"/frag/big.bin"].Source.Derived; r == nil || len(r.Runs) < 20 {
		t.Errorf("the fragmented file's provenance holds %v runs", r)
	}
	holes := 0
	for _, r := range got[vol+"/sparse/holes.bin"].Source.Derived.Runs {
		if r.Offset == -1 {
			holes++
		}
	}
	if holes == 0 {
		t.Error("the sparse file's provenance has no hole run")
	}
	if o, cl := got[vol+"/clone/orig.bin"].Source.Derived, got[vol+"/clone/copy.bin"].Source.Derived; !sameRunsE(o.Runs, cl.Runs) || len(o.Runs) == 0 {
		t.Errorf("clone runs %v differ from the original's %v", cl.Runs, o.Runs)
	}

	// A path inside the snapshot yields the snapshot's bytes, with its provenance.
	snap := want.Snapshots[0]
	snapSHA := map[string]string{}
	for _, n := range snap.Tree {
		snapSHA[n.Path] = n.Sha256
	}
	liveSHA := map[string]string{}
	for _, n := range want.Live.Tree {
		liveSHA[n.Path] = n.Sha256
	}
	if snapSHA["/hello.txt"] == liveSHA["/hello.txt"] || snapSHA["/hello.txt"] == "" {
		t.Fatalf("the oracle's snapshot and live /hello.txt must differ: %q %q", snapSHA["/hello.txt"], liveSHA["/hello.txt"])
	}
	s2, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	ssum := extractAll(t, s2, examine.ExtractOptions{Partition: -1, Paths: []string{vol + "/.snapshots/" + snap.Name + "/hello.txt"}})
	sgot := extractedByPath(ssum)
	sr := sgot[vol+"/.snapshots/"+snap.Name+"/hello.txt"]
	if sr.SHA256 != snapSHA["/hello.txt"] {
		t.Errorf("snapshot hello.txt sha256 %s, oracle %s", sr.SHA256, snapSHA["/hello.txt"])
	}
	if sn := sr.Source.Derived.Snapshot; sn == nil || sn.Name != snap.Name || sn.Xid != snap.Xid {
		t.Errorf("snapshot provenance = %+v, want {%s %d}", sn, snap.Name, snap.Xid)
	}
	if ssum.Files != 1 || ssum.Skipped != 0 || ssum.FSWarnings != 0 {
		t.Errorf("snapshot extract summary = %+v", ssum)
	}
	// The file deleted after the snapshot is only in the snapshot.
	if r := extractAll(t, s2, examine.ExtractOptions{Partition: -1, Paths: []string{vol + "/.snapshots/" + snap.Name + "/many/entry-0001.txt"}}); r.Files != 1 {
		t.Errorf("the file deleted after the snapshot is not extractable from it: %+v", r)
	}

	// Unallocated space: exactly the oracle's free ranges.
	var wantRuns []evidence.Run
	for _, r := range want.FreeRanges {
		wantRuns = append(wantRuns, evidence.Run{Offset: r[0] * want.Container.BlockSize, Length: (r[1] - r[0] + 1) * want.Container.BlockSize})
	}
	usum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatalf("ExportUnallocated: %v", err)
	}
	if usum.FSWarnings != 0 || usum.Skipped != 0 {
		t.Errorf("unalloc summary = %+v", usum)
	}
	checkUnalloc(t, c, img, usum, wantRuns, "p0-apfs")
	verifyOK(t, c)
}
