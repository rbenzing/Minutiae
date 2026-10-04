package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

const apfsBS = 4096

func apfsUUID(b byte) [16]byte {
	var u [16]byte
	for i := range u {
		u[i] = b + byte(i)
	}
	return u
}

// apfsData is a case-insensitive "Data" volume.
func apfsData(files ...apfstest.File) apfstest.Volume {
	return apfstest.Volume{
		Name: "Data", UUID: apfsUUID(1), Role: 1 << 6, CaseInsensitive: true, NormInsensitive: true, HashedKeys: true,
		Files: files,
	}
}

func apfsImage(vols ...apfstest.Volume) []byte {
	return apfstest.Build(apfstest.Options{Blocks: 4096, Xid: 40, Volumes: vols})
}

func apfsNs(sec int64) uint64 { return uint64(sec) * 1_000_000_000 }

// TestInfoDetectsAPFSPartition runs the default driver registry over a GPT image
// whose partition holds an APFS container.
func TestInfoDetectsAPFSPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	c := newCase(t)
	img := apfsImage(apfsData(apfstest.File{Path: "/hello.txt", Data: []byte("hello")}))
	recs := importImage(t, c, disk(img), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // nil Drivers = detect.Drivers
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d, want 1", len(info.Partitions))
	}
	p := info.Partitions[0]
	if p.FSType != "apfs" || p.FSInfo == nil || p.FSInfo.Type != "apfs" || p.Error != "" {
		t.Errorf("partition = %+v (FSInfo %+v), want an apfs container", p, p.FSInfo)
	}
	if p.FSInfo != nil && len(p.FSInfo.Warnings) != 0 {
		t.Errorf("warnings on a clean container: %q", p.FSInfo.Warnings)
	}
}

// apfsFiles is the tree of the builder-image tests.
func apfsFiles() []apfstest.File {
	return []apfstest.File{
		{Path: "/docs", Dir: true, Mode: 0o750},
		{Path: "/docs/a.txt", Data: []byte("hello"), Mode: 0o640, Times: apfstest.Times{Create: apfsNs(1_600_000_000), Modify: apfsNs(1_600_000_100), Change: apfsNs(1_600_000_200), Access: apfsNs(1_600_000_300)}},
		{Path: "/docs/big.bin", Data: pattern(7*apfsBS+123, 3), Mode: 0o644, Times: apfstest.Times{Modify: apfsNs(1_650_000_000)}},
		{Path: "/frag.bin", Data: pattern(8*apfsBS, 4), Fragments: 4, Times: apfstest.Times{Modify: apfsNs(1_660_000_000)}},
		{Path: "/orig.bin", Data: pattern(3*apfsBS, 5), Times: apfstest.Times{Modify: apfsNs(1_670_000_000)}},
		{Path: "/clone.bin", Clone: "/orig.bin", Times: apfstest.Times{Modify: apfsNs(1_670_000_001)}},
		{Path: "/docs/link", LinkTo: "/docs/a.txt"},
		{Path: "/sym", Symlink: "docs/a.txt"},
	}
}

func apfsRFC3339(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

// extractedByPath maps the FSPath of each extracted artifact (the sidecar
// run lists excluded) to its record.
func extractedByPath(sum examine.Summary) map[string]evidence.ManifestRecord {
	out := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		if strings.HasSuffix(r.Path, ".runs.jsonl") || r.Source.Derived == nil {
			continue
		}
		out[r.Source.Derived.FSPath] = r
	}
	return out
}

// TestExtractFromAPFSBuilderImage imports a builder container (as a whole-disk
// image) and extracts /Data: hashes, sizes, modification times, ParentSHA256,
// FSType and FSPath equal the builder's source; case verify is OK and no
// filesystem warning is raised.
func TestExtractFromAPFSBuilderImage(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	files := apfsFiles()
	img := apfsImage(apfsData(files...))
	c := newCase(t)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})

	imgSHA := sha256hex(img)
	got := extractedByPath(sum)
	wantFiles := 0
	for _, f := range files {
		if f.Dir {
			continue
		}
		wantFiles++
		p := "/Data" + f.Path
		rec, ok := got[p]
		if !ok {
			t.Errorf("no artifact for %s", p)
			continue
		}
		var content []byte
		switch {
		case f.Symlink != "":
			content = []byte(f.Symlink)
		case f.LinkTo != "":
			for _, o := range files {
				if o.Path == f.LinkTo {
					content = o.Data
				}
			}
		case f.Clone != "":
			for _, o := range files {
				if o.Path == f.Clone {
					content = o.Data
				}
			}
		default:
			content = f.Data
		}
		if rec.SHA256 != sha256hex(content) || rec.Size != int64(len(content)) || !bytes.Equal(readArtifact(t, c, rec), content) {
			t.Errorf("%s: artifact sha256/size %s/%d, want %s/%d", p, rec.SHA256, rec.Size, sha256hex(content), len(content))
		}
		d := rec.Source.Derived
		if d.ParentSHA256 != imgSHA || d.FSType != "apfs" || rec.Incomplete || d.Snapshot != nil {
			t.Errorf("%s: derived %+v incomplete %v", p, d, rec.Incomplete)
		}
		if f.Times.Modify != 0 && f.Symlink == "" && f.LinkTo == "" {
			if want := apfsRFC3339(int64(f.Times.Modify / 1_000_000_000)); d.Times["modified"] != want {
				t.Errorf("%s: modified %q, want %q", p, d.Times["modified"], want)
			}
		}
		if f.Mode != 0 && d.Mode&0o7777 != f.Mode {
			t.Errorf("%s: mode %04o, want %04o", p, d.Mode&0o7777, f.Mode)
		}
	}
	if sum.Files != wantFiles || len(got) != wantFiles || sum.FSWarnings != 0 {
		t.Errorf("Files = %d, artifacts %d, FSWarnings = %d; want %d files and no warnings", sum.Files, len(got), sum.FSWarnings, wantFiles)
	}
	verifyOK(t, c)
}

// File runs recorded in the provenance reproduce the artifact bytes: a
// fragmented file, a file with a hole and a sparse tail, a clone (its runs are
// the original's) and an empty file.
func TestExtractRunsReproduceContentAPFS(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	sparse := pattern(3*apfsBS, 6)
	img := apfsImage(apfsData(
		apfstest.File{Path: "/frag.bin", Data: pattern(8*apfsBS+5, 7), Fragments: 4},
		apfstest.File{Path: "/sparse.bin", Data: sparse, Holes: [][2]int64{{apfsBS, apfsBS}}, SparseTail: 2 * apfsBS},
		apfstest.File{Path: "/orig.bin", Data: pattern(2*apfsBS, 8)},
		apfstest.File{Path: "/clone.bin", Clone: "/orig.bin"},
		apfstest.File{Path: "/empty"},
	))
	s, disc := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})
	if sum.Files != 5 || sum.Skipped != 0 || sum.FSWarnings != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	got := extractedByPath(sum)
	for p, want := range map[string]int{"/Data/frag.bin": 4, "/Data/orig.bin": 1, "/Data/clone.bin": 1} {
		rec := got[p]
		d := rec.Source.Derived
		if d == nil || len(d.Runs) < want {
			t.Errorf("%s: runs %+v, want at least %d", p, d, want)
			continue
		}
		if !bytes.Equal(readImageRuns(disc, d.Runs), readArtifact(t, c, rec)) {
			t.Errorf("%s: runs do not reproduce the artifact", p)
		}
	}
	if o, cl := got["/Data/orig.bin"], got["/Data/clone.bin"]; o.Source.Derived != nil && cl.Source.Derived != nil &&
		!sameRunsE(o.Source.Derived.Runs, cl.Source.Derived.Runs) {
		t.Errorf("clone runs %v differ from the original's %v", cl.Source.Derived.Runs, o.Source.Derived.Runs)
	}
	sp := got["/Data/sparse.bin"]
	d := sp.Source.Derived
	holes := 0
	for _, r := range d.Runs {
		if r.Offset == -1 {
			holes++
		}
	}
	if holes < 2 || sp.Size != 5*apfsBS || !bytes.Equal(readImageRuns(disc, d.Runs), readArtifact(t, c, sp)) {
		t.Errorf("sparse file: size %d, %d hole runs in %v", sp.Size, holes, d.Runs)
	}
	body := readArtifact(t, c, sp)
	if !bytes.Equal(body[apfsBS:2*apfsBS], make([]byte, apfsBS)) || !bytes.Equal(body[3*apfsBS:], make([]byte, 2*apfsBS)) {
		t.Error("hole and sparse tail must read as zeros")
	}
	verifyOK(t, c)
}

func sameRunsE(a, b []evidence.Run) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An encrypted volume is skipped with a warning (its content is ciphertext the
// reader cannot interpret), the other volumes are extracted whole.
func TestExtractAPFSEncryptedVolumeIsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	enc := apfstest.Volume{Name: "Enc", UUID: apfsUUID(9), Encrypted: true, Files: []apfstest.File{{Path: "/secret", Data: pattern(apfsBS, 1)}}}
	img := apfsImage(apfsData(apfstest.File{Path: "/a.txt", Data: []byte("a")}), enc)
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 1 || len(sum.Warnings) != 1 || sum.Warnings[0].Path != "/Enc" {
		t.Fatalf("summary = %+v, want /Data/a.txt extracted and /Enc skipped with one warning", sum)
	}
	if got := extractedByPath(sum); string(readArtifact(t, c, got["/Data/a.txt"])) != "a" {
		t.Errorf("artifacts = %v", got)
	}
	var enough bool
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); strings.Contains(r, "encrypt") {
			enough = true
		}
	}
	if !enough {
		t.Errorf("no analysis.warning names the encryption: %v", auditByAction(t, c, "analysis.warning"))
	}
	verifyOK(t, c)
}

// A path inside a snapshot is extracted with the snapshot's bytes; the
// provenance records the snapshot path and the snapshot (name and xid); the
// live file is separate. A recursive run of the volume does not include the
// snapshot (it notes that it was skipped).
func TestExtractAPFSSnapshotPath(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	v := apfsData(
		apfstest.File{Path: "/a.txt", Data: []byte("new bytes")},
		apfstest.File{Path: "/live.txt", Data: []byte("live")},
	)
	v.Snapshots = []apfstest.Snapshot{{Name: "S", Files: []apfstest.File{{Path: "/a.txt", Data: []byte("old bytes!")}}}}
	s, _ := imageSession(t, apfsImage(v))
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data/.snapshots/S/a.txt", "/Data/a.txt"}})
	got := extractedByPath(sum)
	snap, live := got["/Data/.snapshots/S/a.txt"], got["/Data/a.txt"]
	if string(readArtifact(t, c, snap)) != "old bytes!" || string(readArtifact(t, c, live)) != "new bytes" {
		t.Fatalf("snapshot %q, live %q", readArtifact(t, c, snap), readArtifact(t, c, live))
	}
	if sn := snap.Source.Derived.Snapshot; sn == nil || sn.Name != "S" || sn.Xid != 2 {
		t.Errorf("snapshot provenance = %+v, want {S 2}", sn)
	}
	if live.Source.Derived.Snapshot != nil {
		t.Errorf("a live extract records a snapshot: %+v", live.Source.Derived.Snapshot)
	}
	if sum.FSWarnings != 0 || sum.Skipped != 0 {
		t.Errorf("summary = %+v", sum)
	}

	// -r of the volume: live files only, plus one note about the skipped snapshots.
	s2, _ := imageSession(t, apfsImage(v))
	sum = extractAll(t, s2, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})
	if g := extractedByPath(sum); len(g) != 2 || g["/Data/a.txt"].Path == "" || g["/Data/live.txt"].Path == "" {
		t.Errorf("recursive extract of the volume = %v, want the two live files", g)
	}
	notes := 0
	for _, e := range auditByAction(t, s2.Case, "analysis.warning") {
		if e.Details["source"] == "examine" && e.Details["path"] == "/Data/.snapshots" {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d notes about the skipped snapshots, want 1", notes)
	}
	verifyOK(t, s2.Case)
	verifyOK(t, c)
}

// Names of 255 bytes, the most APFS stores in a directory record, extract
// without aborting the run: the local component is shortened (with a hash
// suffix) and the provenance keeps the original path.
func TestExtractAPFSLongNames(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	ascii := strings.Repeat("a", 251) + ".txt"
	cjk := strings.Repeat("日", 85) // 255 bytes
	dir := strings.Repeat("d", 255)
	img := apfsImage(apfsData(
		apfstest.File{Path: "/" + ascii, Data: []byte("one")},
		apfstest.File{Path: "/" + cjk, Data: pattern(5000, 1)},
		apfstest.File{Path: "/" + dir, Dir: true},
		apfstest.File{Path: "/" + dir + "/" + ascii, Data: []byte("four")},
	))
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})
	if sum.Files != 3 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Artifacts) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	want := map[string][]byte{
		"/Data/" + ascii:             []byte("one"),
		"/Data/" + cjk:               pattern(5000, 1),
		"/Data/" + dir + "/" + ascii: []byte("four"),
	}
	locals := map[string]string{}
	for _, r := range sum.Artifacts {
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 {
				t.Errorf("component of %d bytes in %q", len(comp), r.Path)
			}
		}
		w, ok := want[r.Source.RemotePath]
		if !ok || !bytes.Equal(readArtifact(t, c, r), w) {
			t.Errorf("artifact %q (remote %q) content wrong", r.Path, r.Source.RemotePath)
		}
		if r.Source.Derived == nil || r.Source.Derived.FSPath != r.Source.RemotePath || r.Source.Derived.FSType != "apfs" {
			t.Errorf("provenance lost the original path: %+v", r.Source)
		}
		if prev, dup := locals[r.Path]; dup {
			t.Errorf("artifacts %q and %q share the local path %q", prev, r.Source.RemotePath, r.Path)
		}
		locals[r.Path] = r.Source.RemotePath
	}
	verifyOK(t, c)
}

// Empty files and files that have no extent records (all hole) are extracted
// whole: an empty file has nil runs and no warning, a file that is one big hole
// is zeros recorded as a hole run, and a symlink is its target text.
func TestExtractAPFSEmptyAndSparseFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	img := apfsImage(apfsData(
		apfstest.File{Path: "/EMPTY.TXT"},
		apfstest.File{Path: "/HOLE.BIN", Size: 3 * apfsBS},
		apfstest.File{Path: "/LINK", Symlink: "EMPTY.TXT"},
		apfstest.File{Path: "/FULL.BIN", Data: pattern(6000, 2)},
	))
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})
	if sum.Files != 4 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Warnings) != 0 || len(sum.Artifacts) != 4 {
		t.Fatalf("summary = %+v", sum)
	}
	got := extractedByPath(sum)
	if e := got["/Data/EMPTY.TXT"]; e.Size != 0 || e.Incomplete || e.Source.Derived.Runs != nil || e.Source.Derived.RunsArtifact != "" {
		t.Errorf("empty file = size %d, derived %+v; want an empty artifact with nil runs", e.Size, e.Source.Derived)
	}
	h := got["/Data/HOLE.BIN"]
	if !bytes.Equal(readArtifact(t, c, h), make([]byte, 3*apfsBS)) || h.Incomplete {
		t.Errorf("all-hole file: size %d incomplete %v", h.Size, h.Incomplete)
	}
	for _, r := range h.Source.Derived.Runs {
		if r.Offset != -1 {
			t.Errorf("all-hole file records data run %+v", r)
		}
	}
	if l := got["/Data/LINK"]; string(readArtifact(t, c, l)) != "EMPTY.TXT" || l.Source.Derived.Runs != nil {
		t.Errorf("symlink = %q, derived %+v", readArtifact(t, c, l), l.Source.Derived)
	}
	if f := got["/Data/FULL.BIN"]; !bytes.Equal(readArtifact(t, c, f), pattern(6000, 2)) || len(f.Source.Derived.Runs) == 0 {
		t.Errorf("file with data: runs %+v", f.Source.Derived)
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}

// A file whose extent list is broken (the second extent points outside the
// container) is extracted as the trusted prefix, flagged incomplete, with the
// runs of that prefix recorded; its intact neighbour is not affected.
func TestExtractAPFSBrokenExtentsIsIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end tests are skipped under -short")
	}
	content := pattern(4*apfsBS, 3)
	img := apfsImage(apfsData(
		apfstest.File{Path: "/BROKEN.BIN", Data: content, Extents: []apfstest.Extent{
			{Logical: 0, Length: 2 * apfsBS, Rel: true},
			{Logical: 2 * apfsBS, Length: 2 * apfsBS, Phys: 1 << 50},
		}},
		apfstest.File{Path: "/FINE.TXT", Data: []byte("fine")},
	))
	s, disc := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/Data"}, Recursive: true})
	got := extractedByPath(sum)
	broken, fine := got["/Data/BROKEN.BIN"], got["/Data/FINE.TXT"]
	if sum.Files != 1 || len(got) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if !broken.Incomplete || broken.Size != 2*apfsBS || !bytes.Equal(readArtifact(t, c, broken), content[:2*apfsBS]) {
		t.Errorf("broken artifact: incomplete=%v size=%d, want an incomplete 2-block prefix", broken.Incomplete, broken.Size)
	}
	d := broken.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs were not recorded: %+v", d)
	}
	var covered int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(disc)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(disc))
		}
		covered += r.Length
	}
	if covered != broken.Size || !bytes.Equal(readImageRuns(disc, d.Runs), readArtifact(t, c, broken)) {
		t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, broken.Size)
	}
	if fine.Incomplete || string(readArtifact(t, c, fine)) != "fine" {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	if sum.Skipped != 1 || sum.FSWarnings < 1 {
		t.Errorf("Skipped = %d, FSWarnings = %d; want 1 skip (read failed) and the reader's warning", sum.Skipped, sum.FSWarnings)
	}
	var reasons []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
	}
	joined := strings.Join(reasons, "\n")
	if strings.Contains(joined, "no runs recorded") || !strings.Contains(joined, "partial artifact kept, flagged incomplete") {
		t.Errorf("warnings do not explain the partial extraction: %q", reasons)
	}
	verifyOK(t, c)
}

// apfsFixtureOracle is the part of internal/filesys/apfs/testdata/*.expect.json
// used here (from tools/fixtures/apfs_oracle.py, an independent decoder).
type apfsFixtureOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Container struct {
		BlockSize int64 `json:"block_size"`
	} `json:"container"`
	FreeRanges [][2]int64 `json:"free_ranges"` // inclusive block ranges
}

func loadAPFSFixture(t *testing.T, name string) ([]byte, apfsFixtureOracle) {
	t.Helper()
	dir := filepath.Join("..", "filesys", "apfs", "testdata")
	gz, err := os.ReadFile(filepath.Join(dir, name+".img.gz"))
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
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o apfsFixtureOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s: the oracle is stale, regenerate the fixtures", got, o.Generator.ImageSHA256)
	}
	return img, o
}

// TestUnallocAPFSFixture exports the unallocated space of the real empty
// containers made by mkapfs: the runs are exactly the free ranges of the
// independent oracle, no filesystem warning is raised, and case verify is OK.
func TestUnallocAPFSFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("exports about 60 MiB per image")
	}
	for _, name := range []string{"apfs-ci", "apfs-cs"} {
		t.Run(name, func(t *testing.T) {
			img, want := loadAPFSFixture(t, name)
			c := newCase(t)
			recs := importImage(t, c, img, 1)
			s, err := examine.Open(c, recs[0].ID, examine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			var wantRuns []evidence.Run
			for _, r := range want.FreeRanges {
				wantRuns = append(wantRuns, evidence.Run{Offset: r[0] * want.Container.BlockSize, Length: (r[1] - r[0] + 1) * want.Container.BlockSize})
			}
			if len(wantRuns) == 0 {
				t.Fatal("the oracle lists no free space")
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
		})
	}
}

// TestExtractFromAPFSFixture extracts the real empty containers: the volume
// holds no file, so no file artifact is written, and nothing is warned about.
func TestExtractFromAPFSFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports a 64 MiB image")
	}
	img, _ := loadAPFSFixture(t, "apfs-ci")
	c := newCase(t)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 0 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Artifacts) != 0 || len(sum.Warnings) != 0 {
		t.Errorf("summary = %+v, want nothing extracted and no warning", sum)
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for an empty container: %v", w)
	}
	verifyOK(t, c)
}
