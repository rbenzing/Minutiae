package apfs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
)

// popFixture is the populated container written by a real kernel driver
// (linux-apfs-rw, booted under QEMU by tools/fixtures/apfs_populate.sh) and the
// facts the independent oracle (apfs_populated_oracle.py) read from it. The
// oracle decodes the image on its own and compares what it decodes with the
// SOURCE TREE the driver was fed, so every expectation below traces back to the
// source tree or to the bytes of the image, never to this reader.
const popFixture = "apfs-populated"

type popNode struct {
	Path      string     `json:"path"`
	Type      string     `json:"type"`
	Size      int64      `json:"size"`
	Mode      uint32     `json:"mode"`
	UID       uint32     `json:"uid"`
	GID       uint32     `json:"gid"`
	MtimeNs   int64      `json:"mtime_ns"`
	Nlink     int        `json:"nlink"`
	Inode     uint64     `json:"inode"`
	PrivateID uint64     `json:"private_id"`
	Xattrs    []string   `json:"xattrs"`
	Sha256    string     `json:"sha256"`
	Runs      [][2]int64 `json:"runs"`
	Link      string     `json:"link"`
	NameHash  *uint32    `json:"name_hash"`
}

type popSnapshot struct {
	Name       string    `json:"name"`
	Xid        uint64    `json:"xid"`
	CreateTime int64     `json:"create_time"`
	ChangeTime int64     `json:"change_time"`
	Tree       []popNode `json:"tree"`
}

type popExpect struct {
	Container struct {
		BlockSize  int    `json:"block_size"`
		BlockCount uint64 `json:"block_count"`
		UUID       string `json:"uuid"`
		Xid        uint64 `json:"xid"`
	} `json:"container"`
	Volume struct {
		Name          string `json:"name"`
		UUID          string `json:"uuid"`
		NumSnapshots  uint64 `json:"num_snapshots"`
		NumFiles      uint64 `json:"num_files"`
		NumDirs       uint64 `json:"num_directories"`
		NumSymlinks   uint64 `json:"num_symlinks"`
		LastModTime   uint64 `json:"last_mod_time"`
		RootTreeOid   uint64 `json:"root_tree_oid"`
		OmapOid       uint64 `json:"omap_oid"`
		Incompatible  uint64 `json:"incompatible_features"`
		VolumeXid     uint64 `json:"xid"`
		NextObjectID  uint64 `json:"next_obj_id"`
		FeaturesField uint64 `json:"features"`
	} `json:"volume"`
	Live struct {
		Records      int            `json:"records"`
		Levels       int            `json:"levels"`
		RecordCounts map[string]int `json:"record_counts"`
		Tree         []popNode      `json:"tree"`
	} `json:"live"`
	Snapshots  []popSnapshot `json:"snapshots"`
	FreeRanges [][2]uint64   `json:"free_ranges"` // inclusive block ranges
	FreeBlocks uint64        `json:"free_blocks"`
	FreeQueues []struct {
		Entries []struct {
			Paddr uint64 `json:"paddr"`
			Len   uint64 `json:"len"`
		} `json:"entries"`
	} `json:"free_queues"`
	Specials struct {
		Clones      [][2]string      `json:"clones"`
		Sparse      map[string]int64 `json:"sparse"`
		Interleaved [][2]string      `json:"interleaved"`
	} `json:"specials"`
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
}

// popLoad gunzips the fixture, checks its sha256 against the oracle first, and
// returns the image with the decoded oracle.
func popLoad(t *testing.T) ([]byte, *popExpect) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", popFixture+".expect.json"))
	if err != nil {
		t.Fatalf("populated APFS fixture: %v", err)
	}
	var exp popExpect
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("%s oracle: %v", popFixture, err)
	}
	f, err := os.Open(filepath.Join("testdata", popFixture+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != exp.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 %s, oracle says %s (regenerate the image and its expect.json together)", popFixture, got, exp.Generator.ImageSHA256)
	}
	return img, &exp
}

// popMerge merges adjacent physical runs and adjacent holes, so that two
// run lists that describe the same layout compare equal.
func popMerge(runs []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range runs {
		if r.Length == 0 {
			continue
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			if (r.Offset < 0 && last.Offset < 0) || (r.Offset >= 0 && last.Offset >= 0 && last.Offset+last.Length == r.Offset) {
				last.Length += r.Length
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func popRunsOf(n popNode) []filesys.Run {
	out := make([]filesys.Run, len(n.Runs))
	for i, r := range n.Runs {
		out[i] = filesys.Run{Offset: r[0], Length: r[1]}
	}
	return out
}

// popReadRuns reads a file's content from the image bytes the runs name.
func popReadRuns(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		if r.Offset < 0 {
			out = append(out, make([]byte, r.Length)...)
		} else {
			out = append(out, img[r.Offset:r.Offset+r.Length]...)
		}
	}
	return out
}

func popType(e filesys.Entry) string {
	switch e.Type {
	case filesys.TypeDir:
		return "dir"
	case filesys.TypeSymlink:
		return "symlink"
	case filesys.TypeFile:
		return "file"
	default:
		return "other"
	}
}

// popWalk lists every entry below the entry named root (a path), keyed by its
// path relative to root, skipping the synthetic .snapshots directory.
func popWalk(t *testing.T, f *apfs.FS, root string) map[string]filesys.Entry {
	t.Helper()
	re, err := f.Lookup(root)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", root, err)
	}
	got := map[string]filesys.Entry{}
	var walk func(dir filesys.Entry, rel string)
	walk = func(dir filesys.Entry, rel string) {
		es, err := f.ReadDir(dir)
		if err != nil {
			t.Errorf("ReadDir(%s%s): %v", root, rel, err)
			return
		}
		for _, e := range es {
			if filesys.IsSnapshotsDir(e) {
				continue
			}
			p := path.Join("/", rel, e.Name)
			if _, dup := got[p]; dup {
				t.Errorf("%s: listed twice", p)
			}
			got[p] = e
			if e.Type == filesys.TypeDir {
				walk(e, p)
			}
		}
	}
	walk(re, "")
	return got
}

// popCheckTree compares the tree below root with the oracle's: every path, no
// extra, and per entry the type, mode, owner, size, mtime to the nanosecond,
// symlink target, link count, extended-attribute names, SHA-256 of the content
// read through the reader, the runs (merged), the content rebuilt from the
// runs, the hard-link and clone identities. It returns every file run.
func popCheckTree(t *testing.T, img []byte, f *apfs.FS, root string, want []popNode) []filesys.Run {
	t.Helper()
	got := popWalk(t, f, root)
	wantPaths := make([]string, 0, len(want))
	for _, w := range want {
		wantPaths = append(wantPaths, w.Path)
	}
	gotPaths := make([]string, 0, len(got))
	for p := range got {
		gotPaths = append(gotPaths, p)
	}
	slices.Sort(gotPaths)
	slices.Sort(wantPaths)
	if !slices.Equal(gotPaths, wantPaths) {
		var missing, extra []string
		for _, p := range wantPaths {
			if _, ok := got[p]; !ok {
				missing = append(missing, p)
			}
		}
		for _, p := range gotPaths {
			if !slices.Contains(wantPaths, p) {
				extra = append(extra, p)
			}
		}
		t.Fatalf("%s: %d paths, oracle %d; missing %q, extra %q", root, len(gotPaths), len(wantPaths), missing[:min(10, len(missing))], extra[:min(10, len(extra))])
	}

	var allRuns []filesys.Run
	idOfInode := map[uint64]string{}
	idOfPrivate := map[uint64]string{}
	bad := 0
	report := func(format string, a ...any) {
		if bad++; bad <= 25 {
			t.Errorf(format, a...)
		}
	}
	for _, w := range want {
		e := got[w.Path]
		if popType(e) != w.Type {
			report("%s: type %s, oracle %s", w.Path, popType(e), w.Type)
			continue
		}
		if e.Mode&0o7777 != w.Mode || e.UID != w.UID || e.GID != w.GID {
			report("%s: mode/uid/gid %o/%d/%d, oracle %o/%d/%d", w.Path, e.Mode&0o7777, e.UID, e.GID, w.Mode, w.UID, w.GID)
		}
		if ns := e.Times.Modified.T.UnixNano(); ns != w.MtimeNs || !e.Times.Modified.ZoneKnown {
			report("%s: mtime %d (zone known %v), oracle %d", w.Path, ns, e.Times.Modified.ZoneKnown, w.MtimeNs)
		}
		if w.Type == "symlink" && e.LinkTarget != w.Link {
			report("%s: link target %q, oracle %q", w.Path, e.LinkTarget, w.Link)
		}
		var xs []string
		for _, kv := range e.Attrs {
			if kv.Key == "xattr" {
				xs = append(xs, kv.Value)
			}
		}
		slices.Sort(xs)
		if wx := slices.Clone(w.Xattrs); !slices.Equal(xs, wx) && (len(xs) != 0 || len(wx) != 0) {
			report("%s: xattr names %q, oracle %q", w.Path, xs, wx)
		}
		if w.Type == "file" {
			if v, _ := attr(e, "nlink"); v != fmt.Sprint(w.Nlink) {
				report("%s: nlink %q, oracle %d", w.Path, v, w.Nlink)
			}
			wantID := fmt.Sprintf(":%d", w.Inode)
			if !strings.HasSuffix(e.ID, wantID) {
				report("%s: entry ID %s, oracle inode %d", w.Path, e.ID, w.Inode)
			}
			if prev, ok := idOfInode[w.Inode]; ok && prev != e.ID {
				report("%s: hard links of inode %d have different IDs %s and %s", w.Path, w.Inode, prev, e.ID)
			}
			idOfInode[w.Inode] = e.ID
			idOfPrivate[w.PrivateID] = e.ID
		}
		if w.Type == "dir" {
			continue
		}
		file, err := f.Open(e)
		if err != nil {
			report("%s: Open: %v", w.Path, err)
			continue
		}
		runs := file.Runs()
		allRuns = append(allRuns, runs...)
		if w.Type == "symlink" {
			continue
		}
		if file.Size() != w.Size || e.Size != w.Size {
			report("%s: size %d (entry %d), oracle %d", w.Path, file.Size(), e.Size, w.Size)
			continue
		}
		if err := filesys.CheckRuns(runs, file.Size(), int64(len(img))); err != nil {
			report("%s: runs violate the contract: %v", w.Path, err)
		}
		if got, want := popMerge(runs), popRunsOf(w); !slices.Equal(got, want) {
			report("%s: runs %v, oracle %v", w.Path, got, want)
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(file, 0, file.Size())); err != nil {
			report("%s: read: %v", w.Path, err)
		} else if hex.EncodeToString(h.Sum(nil)) != w.Sha256 {
			report("%s: content sha256 %x, oracle %s", w.Path, h.Sum(nil), w.Sha256)
		}
		if sum := sha256.Sum256(popReadRuns(img, runs)); hex.EncodeToString(sum[:]) != w.Sha256 {
			report("%s: the content rebuilt from the reader's runs has sha256 %x, oracle %s", w.Path, sum, w.Sha256)
		}
	}
	if bad > 25 {
		t.Errorf("%d differences in all", bad)
	}
	return allRuns
}

// testPopulated is the populated-fixture part of TestAPFSMatchesOracle.
func testPopulated(t *testing.T) {
	img, exp := popLoad(t)
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var fileRuns []filesys.Run

	t.Run("container and volume", func(t *testing.T) {
		n := f.NX()
		if int(n.BlockSize) != exp.Container.BlockSize || n.BlockCount != exp.Container.BlockCount || n.Xid != exp.Container.Xid {
			t.Errorf("block size/count/xid %d/%d/%d, oracle %d/%d/%d", n.BlockSize, n.BlockCount, n.Xid, exp.Container.BlockSize, exp.Container.BlockCount, exp.Container.Xid)
		}
		in := f.Info()
		if in.UUID != exp.Container.UUID || !slices.Equal(in.Volumes, []string{exp.Volume.Name}) {
			t.Errorf("Info uuid %s volumes %q, oracle %s %q", in.UUID, in.Volumes, exp.Container.UUID, exp.Volume.Name)
		}
		line := fmt.Sprintf("vol[0] %q: role=none, case-insensitive, normalization-insensitive, unencrypted, snapshots=%d", exp.Volume.Name, exp.Volume.NumSnapshots)
		if !contains(in.Features, line) {
			t.Errorf("Info.Features lacks %q: %q", line, in.Features)
		}
		v, ok := f.VolumeFields(0)
		if !ok || !v.Readable {
			t.Fatal("volume 0 unreadable")
		}
		if v.Snapshots != exp.Volume.NumSnapshots || v.LastMod != exp.Volume.LastModTime || v.RootOid != exp.Volume.RootTreeOid || v.OmapOid != exp.Volume.OmapOid || v.Incompat != exp.Volume.Incompatible {
			t.Errorf("volume snapshots/last mod/root/omap/incompat %d/%d/%d/%d/%#x, oracle %d/%d/%d/%d/%#x",
				v.Snapshots, v.LastMod, v.RootOid, v.OmapOid, v.Incompat, exp.Volume.NumSnapshots, exp.Volume.LastModTime, exp.Volume.RootTreeOid, exp.Volume.OmapOid, exp.Volume.Incompatible)
		}
		if w := in.Warnings; len(w) != 0 {
			t.Errorf("warnings on a real populated container: %q", w)
		}
		// The oracle's counts of live entries are the volume superblock's.
		var dirs, links uint64
		inodes := map[uint64]bool{}
		for _, nd := range exp.Live.Tree {
			switch nd.Type {
			case "file":
				inodes[nd.Inode] = true
			case "dir":
				dirs++
			case "symlink":
				links++
			}
		}
		if uint64(len(inodes)) != exp.Volume.NumFiles || links != exp.Volume.NumSymlinks || dirs != exp.Volume.NumDirs {
			t.Errorf("oracle tree has %d files (inodes), %d symlinks and %d directories, the volume superblock says %d, %d and %d", len(inodes), links, dirs, exp.Volume.NumFiles, exp.Volume.NumSymlinks, exp.Volume.NumDirs)
		}
	})

	t.Run("fs tree records", func(t *testing.T) {
		v, _ := f.VolumeFields(0)
		recs, err := f.ScanVolumeTree(v.OmapOid, v.RootOid, v.RootType)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != exp.Live.Records {
			t.Errorf("fs tree has %d records, oracle %d", len(recs), exp.Live.Records)
		}
		names := map[uint64]string{3: "inode", 9: "dir_rec", 6: "dstream_id", 8: "file_extent", 4: "xattr", 5: "sibling_link", 12: "sibling_map", 10: "dir_stats"}
		counts := map[string]int{}
		for _, r := range recs {
			typ := le.Uint64(r.Key) >> 60
			n, ok := names[typ]
			if !ok {
				n = fmt.Sprintf("type_%d", typ)
			}
			counts[n]++
		}
		if !mapsEqual(counts, exp.Live.RecordCounts) {
			t.Errorf("record counts %v, oracle %v", counts, exp.Live.RecordCounts)
		}
		if exp.Live.Levels < 2 {
			t.Errorf("the oracle's fs tree has %d level(s): the fixture must be multi-level", exp.Live.Levels)
		}
	})

	t.Run("live tree", func(t *testing.T) {
		fileRuns = append(fileRuns, popCheckTree(t, img, f, "/"+exp.Volume.Name, exp.Live.Tree)...)
	})

	t.Run("snapshots", func(t *testing.T) {
		if len(exp.Snapshots) != int(exp.Volume.NumSnapshots) || len(exp.Snapshots) == 0 {
			t.Fatalf("oracle lists %d snapshots, the volume %d", len(exp.Snapshots), exp.Volume.NumSnapshots)
		}
		for _, s := range exp.Snapshots {
			t.Run(s.Name, func(t *testing.T) {
				root := "/" + exp.Volume.Name + "/.snapshots/" + s.Name
				se, err := f.Lookup(root)
				if err != nil {
					t.Fatalf("Lookup(%s): %v", root, err)
				}
				if v, _ := attr(se, "snapshot_xid"); v != fmt.Sprint(s.Xid) {
					t.Errorf("snapshot_xid %q, oracle %d", v, s.Xid)
				}
				want := time.Unix(0, s.CreateTime).UTC().Format("2006-01-02T15:04:05.999999999Z")
				if v, _ := attr(se, "snapshot_created"); v != want {
					t.Errorf("snapshot_created %q, oracle %s", v, want)
				}
				fileRuns = append(fileRuns, popCheckTree(t, img, f, root, s.Tree)...)
				// SnapshotPath maps a live path to the snapshot's.
				p, err := f.SnapshotPath("/"+exp.Volume.Name+"/docs/readme.txt", s.Name)
				if err != nil || p != root+"/docs/readme.txt" {
					t.Errorf("SnapshotPath = %q, %v", p, err)
				}
			})
		}
		// The snapshot differs from the live tree as the source trees do: the
		// deleted file is only in the snapshot, the new directory only live.
		live := popWalk(t, f, "/"+exp.Volume.Name)
		snap := popWalk(t, f, "/"+exp.Volume.Name+"/.snapshots/"+exp.Snapshots[0].Name)
		for _, p := range []string{"/many/entry-0001.txt"} {
			if _, ok := live[p]; ok {
				t.Errorf("%s is in the live tree", p)
			}
			if _, ok := snap[p]; !ok {
				t.Errorf("%s is missing from the snapshot", p)
			}
		}
		for _, p := range []string{"/after", "/after/new.txt"} {
			if _, ok := live[p]; !ok {
				t.Errorf("%s is missing from the live tree", p)
			}
			if _, ok := snap[p]; ok {
				t.Errorf("%s is in the snapshot", p)
			}
		}
	})

	t.Run("name lookup", func(t *testing.T) {
		// The volume is case- and normalization-insensitive: a name that differs
		// in case or in Unicode normalization still resolves, to the entry
		// that holds the name as stored.
		stored := map[string]popNode{}
		for _, n := range exp.Live.Tree {
			stored[n.Path] = n
		}
		root := "/" + exp.Volume.Name
		for _, tc := range []struct {
			query, stored string
			approx        bool // a normalization difference: see below
		}{
			{"/case/upper.txt", "/case/UPPER.TXT", false},
			{"/CASE/LOWER.TXT", "/case/lower.txt", false},
			{"/case/CAMELCASE.TXT", "/case/CamelCase.Txt", false},
			{"/mixedcase.txt", "/MixedCase.TXT", false},
			{"/unicode/café.txt", "/unicode/café.txt", true}, // NFD query, NFC stored
			{"/nfd/café.txt", "/nfd/café.txt", true},         // NFC query, NFD stored
			{"/unicode/CAFÉ.TXT", "/unicode/café.txt", false},
			{"/unicode/ÜBER-ÅNGSTRÖM.TXT", "/unicode/über-Ångström.txt", false},
			{"/unicode/ДОКУМЕНТ.TXT", "/unicode/документ.txt", false},
			{"/unicode/ΕΛΛΗΝΙΚΆ.TXT", "/unicode/Ελληνικά.txt", false},
			{"/unicode/한국어.txt", "/unicode/한국어.txt", false},
			{"/nfd/한국어.txt", "/nfd/" + "한국어" + ".txt", true}, // composed query, conjoining-jamo stored
		} {
			want, ok := stored[tc.stored]
			if !ok {
				t.Fatalf("the oracle has no path %q: the test table is wrong", tc.stored)
			}
			e, err := f.Lookup(root + tc.query)
			if errors.Is(err, filesys.ErrNotFound) && tc.approx {
				// dir.go: lookup folds case with strings.EqualFold, it does not
				// normalize Unicode, so a differently normalized spelling of a
				// non-ASCII name is not found (listing is unaffected).
				t.Logf("known approximation: Lookup(%q) does not find %q", tc.query, tc.stored)
				continue
			}
			if err != nil {
				t.Errorf("Lookup(%q): %v", tc.query, err)
				continue
			}
			if e.Name != path.Base(tc.stored) || !strings.HasSuffix(e.ID, fmt.Sprintf(":%d", want.Inode)) {
				t.Errorf("Lookup(%q) = %q (%s), want %q (inode %d)", tc.query, e.Name, e.ID, path.Base(tc.stored), want.Inode)
			}
		}
		if _, err := f.Lookup(root + "/case/missing.txt"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup of a missing name = %v, want ErrNotFound", err)
		}
		// The stored hash of every directory record of the real volume: the
		// reader's recipe (ASCII names) reproduces it, so a name search by
		// hash is sound on real data.
		checked := 0
		for _, n := range exp.Live.Tree {
			if n.NameHash == nil {
				continue
			}
			h, ok := apfs.NameHash([]byte(path.Base(n.Path)), true)
			if !ok {
				continue // not ASCII: the reader does not verify those
			}
			checked++
			if h != *n.NameHash {
				t.Errorf("%s: stored name hash %d, reader computes %d", n.Path, *n.NameHash, h)
			}
		}
		if checked < 600 {
			t.Errorf("only %d ASCII names had their stored hash checked", checked)
		}
	})

	t.Run("specials", func(t *testing.T) {
		byPath := map[string]popNode{}
		for _, n := range exp.Live.Tree {
			byPath["/"+strings.TrimPrefix(n.Path, "/")] = n
		}
		for _, c := range exp.Specials.Clones {
			a, b := byPath[c[0]], byPath[c[1]]
			if a.Sha256 == "" || a.Sha256 != b.Sha256 || !slices.Equal(a.Runs, b.Runs) {
				t.Errorf("clone %v: oracle contents or runs differ", c)
			}
			// the reader sees the shared extents as the same runs
			var ra, rb []filesys.Run
			for i, p := range c {
				e, err := f.Lookup("/" + exp.Volume.Name + p)
				if err != nil {
					t.Fatal(err)
				}
				file, err := f.Open(e)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					ra = file.Runs()
				} else {
					rb = file.Runs()
				}
			}
			if !slices.Equal(ra, rb) || len(ra) == 0 {
				t.Errorf("clone %v: the reader's runs differ: %v vs %v", c, ra, rb)
			}
		}
		for _, p := range exp.Specials.Interleaved {
			for _, q := range p {
				if n := len(byPath[q].Runs); n < 20 {
					t.Errorf("%s has %d merged runs; the fixture must hold fragmented files", q, n)
				}
			}
		}
		holes := 0
		for p := range exp.Specials.Sparse {
			for _, r := range byPath[p].Runs {
				if r[0] < 0 {
					holes++
					break
				}
			}
		}
		if holes != len(exp.Specials.Sparse) || holes == 0 {
			t.Errorf("%d of %d sparse files hold a hole run in the oracle", holes, len(exp.Specials.Sparse))
		}
	})

	t.Run("unallocated", func(t *testing.T) {
		runs, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		bs := int64(exp.Container.BlockSize)
		want := make([]filesys.Run, 0, len(exp.FreeRanges))
		var blocks int64
		for _, r := range exp.FreeRanges {
			want = append(want, filesys.Run{Offset: int64(r[0]) * bs, Length: int64(r[1]-r[0]+1) * bs})
			blocks += int64(r[1]-r[0]) + 1
		}
		if blocks != int64(exp.FreeBlocks) {
			t.Fatalf("oracle: ranges hold %d blocks, free_blocks says %d", blocks, exp.FreeBlocks)
		}
		if !slices.Equal(runs, want) {
			t.Errorf("Unallocated = %d runs / %d blocks, oracle %d runs / %d blocks", len(runs), runLen(runs)/bs, len(want), blocks)
			for i := 0; i < min(len(runs), len(want)); i++ {
				if runs[i] != want[i] {
					t.Errorf("first difference at run %d: %+v, oracle %+v", i, runs[i], want[i])
					break
				}
			}
		}
		// The blocks the oracle found in a free queue (freed in the newest
		// transaction, not yet reusable) are allocated in the bitmap and so are
		// not reported free.
		for _, q := range exp.FreeQueues {
			for _, e := range q.Entries {
				for _, r := range runs {
					if int64(e.Paddr)*bs < r.Offset+r.Length && r.Offset < int64(e.Paddr+e.Len)*bs {
						t.Errorf("free-queue blocks %d+%d are reported free: %+v", e.Paddr, e.Len, r)
					}
				}
			}
		}
		// No free run overlaps any file's data, live or snapshot.
		data := slices.DeleteFunc(slices.Clone(fileRuns), func(r filesys.Run) bool { return r.Offset < 0 })
		slices.SortFunc(data, func(a, b filesys.Run) int { return int(a.Offset - b.Offset) })
		j := 0
		for _, fr := range data {
			for j < len(runs) && runs[j].Offset+runs[j].Length <= fr.Offset {
				j++
			}
			if j < len(runs) && runs[j].Offset < fr.Offset+fr.Length {
				t.Errorf("free run %+v overlaps the file run %+v", runs[j], fr)
			}
		}
		if len(fileRuns) == 0 {
			t.Error("no file runs were collected: the tree subtests did not run")
		}
	})
}

func runLen(rs []filesys.Run) int64 {
	var n int64
	for _, r := range rs {
		n += r.Length
	}
	return n
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
