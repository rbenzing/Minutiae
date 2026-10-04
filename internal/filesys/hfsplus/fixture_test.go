package hfsplus_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
)

// fixtureNames are the committed real-image fixtures: five empty volumes made
// by mkfs.hfsplus and one populated by the Linux hfsplus driver.
var fixtureNames = []string{"hfsplus-empty", "hfsx-empty", "hfsplus-journal", "hfsplus-1k", "hfsplus-wrapped", "hfsplus-populated"}

// fxOracle is the part of a fixture's expect.json the tests below use. The
// oracle (tools/fixtures/hfsplus_oracle.py) is a second parse of the image plus
// fsck.hfsplus and blkid, and, for the populated image, a comparison with the
// source tree the volume was filled from.
type fxOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type          string     `json:"type"`
	Label         string     `json:"label"`
	VolumeID      string     `json:"volume_id"`
	BlockSize     int64      `json:"block_size"`
	Size          int64      `json:"size"`
	ImageSize     int64      `json:"image_size"`
	VolumeOffset  int64      `json:"volume_offset"`
	Journaled     bool       `json:"journaled"`
	CaseSensitive bool       `json:"case_sensitive"`
	Wrapper       *struct{}  `json:"wrapper"`
	TotalBlocks   int64      `json:"total_blocks"`
	FreeCount     int64      `json:"free_count"`
	FreeBlocks    [][2]int64 `json:"free_blocks"`
	Populated     bool       `json:"populated"`
	SystemFiles   map[string]struct {
		Extents [][2]int64 `json:"extents"`
	} `json:"system_files"`
	Files   []fxFile `json:"files"`
	Private *struct {
		Name        string `json:"name"`
		RootIndex   int    `json:"root_leaf_index"`
		RootKids    int    `json:"root_children"`
		PrevSibling string `json:"previous_sibling"`
		Inodes      []struct {
			Name  string `json:"name"`
			Links int    `json:"links"`
		} `json:"inodes"`
	} `json:"private_folder"`
	RootOrder []string `json:"root_children_in_leaf_order"`
	Fragments []string `json:"fragmented_files"`
	Catalog   struct {
		Depth int `json:"depth"`
	} `json:"catalog"`
	ExtentsTree struct {
		LeafRecords int `json:"leaf_records"`
	} `json:"extents_tree"`
}

type fxFile struct {
	Path     string     `json:"path"`
	Type     string     `json:"type"`
	CNID     uint32     `json:"cnid"`
	Mode     uint32     `json:"mode"`
	UID      uint32     `json:"uid"`
	GID      uint32     `json:"gid"`
	Created  *int64     `json:"created"`
	Modified *int64     `json:"modified"`
	Changed  *int64     `json:"changed"`
	Accessed *int64     `json:"accessed"`
	Backup   *int64     `json:"backup"`
	Size     int64      `json:"size"`
	SHA256   string     `json:"sha256"`
	Extents  [][2]int64 `json:"extents"`
	Valence  int        `json:"valence"`
	Target   string     `json:"target"`
	HardLink bool       `json:"hardlink"`
	Links    int        `json:"link_count"`
	LinkIno  int64      `json:"hardlink_inode"`
	InodeID  uint32     `json:"inode_cnid"`
	Private  bool       `json:"private"`
}

func loadOracle(t testing.TB, name string) fxOracle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex fxOracle
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	return ex
}

func tsUnix(ts filesys.Timestamp) *int64 {
	if ts.T.IsZero() {
		return nil
	}
	u := ts.T.Unix()
	return &u
}

func sameUnix(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// expectedRuns turns the oracle's block extents (inline plus overflow, in
// logical order) into the byte runs File.Runs must return: adjacent extents
// merged in file order, cut at the file size.
func expectedRuns(ex *fxOracle, size int64, exts [][2]int64) []filesys.Run {
	var runs []filesys.Run
	left := size
	for _, e := range exts {
		if left <= 0 {
			break
		}
		off, n := ex.VolumeOffset+e[0]*ex.BlockSize, min(e[1]*ex.BlockSize, left)
		left -= n
		if k := len(runs) - 1; k >= 0 && runs[k].Offset+runs[k].Length == off {
			runs[k].Length += n
			continue
		}
		runs = append(runs, filesys.Run{Offset: off, Length: n})
	}
	return runs
}

// Every fixture opens with no warning and equals its oracle: the image hash
// (a stale oracle fails first), Info, the exact live tree (names, types,
// sizes, content, permissions, times, owner, symlink targets, hard links),
// every file's runs in logical order, and the free space, with no free run
// overlapping live data.
func TestHFSPlusMatchesOracle(t *testing.T) {
	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			img, _ := loadFixture(t, name)
			ex := loadOracle(t, name)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != ex.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s, the oracle describes %s (stale oracle)", got, ex.Generator.ImageSHA256)
			}
			if int64(len(img)) != ex.ImageSize {
				t.Fatalf("image is %d bytes, the oracle says %d", len(img), ex.ImageSize)
			}
			f := open(t, img)

			info := f.Info()
			wantFeat := []string{"case-insensitive"}
			if ex.CaseSensitive {
				wantFeat = []string{"case-sensitive"}
			}
			if ex.Journaled {
				wantFeat = append(wantFeat, "journaled")
			}
			if ex.Wrapper != nil {
				wantFeat = append(wantFeat, "hfs-wrapper")
			}
			if info.Type != ex.Type || info.Label != ex.Label || info.UUID != ex.VolumeID ||
				int64(info.BlockSize) != ex.BlockSize || info.Size != ex.VolumeOffset+ex.Size ||
				!slices.Equal(info.Features, wantFeat) || info.Encrypted {
				t.Errorf("Info = %+v\noracle: type %s label %q volume id %s block size %d size %d+%d features %v",
					info, ex.Type, ex.Label, ex.VolumeID, ex.BlockSize, ex.VolumeOffset, ex.Size, wantFeat)
			}

			// --- the live tree
			got := map[string]filesys.Entry{}
			err := filesys.Walk(f, f.Root(), "/", func(path string, e filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("walk %s: %v", path, err)
					return nil
				}
				if e.Deleted {
					t.Errorf("%s: a live listing holds a deleted entry", path)
				}
				got[path] = e
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]fxFile{}
			var wantPaths []string
			for _, o := range ex.Files {
				if o.Path == "/" {
					continue
				}
				p := displayPath(o.Path)
				want[p] = o
				wantPaths = append(wantPaths, p)
			}
			var gotPaths []string
			for p := range got {
				gotPaths = append(gotPaths, p)
			}
			slices.Sort(gotPaths)
			slices.Sort(wantPaths)
			if !slices.Equal(gotPaths, wantPaths) {
				missing, extra := diffStrings(wantPaths, gotPaths)
				t.Fatalf("the live tree differs from the oracle: %d entries vs %d; missing %q, extra %q", len(gotPaths), len(wantPaths), first(missing, 5), first(extra, 5))
			}
			if rootEntry := f.Root(); rootEntry.Type != filesys.TypeDir {
				t.Errorf("root = %+v", rootEntry)
			}

			var live []filesys.Run
			hardlinks := map[uint32][]string{}
			for _, p := range wantPaths {
				o, e := want[p], got[p]
				if e.Type.String() != o.Type {
					t.Errorf("%s: type %s, oracle %s", p, e.Type, o.Type)
					continue
				}
				if e.Mode&0o7777 != o.Mode || e.UID != o.UID || e.GID != o.GID {
					t.Errorf("%s: mode %o uid %d gid %d, oracle %o %d %d", p, e.Mode&0o7777, e.UID, e.GID, o.Mode, o.UID, o.GID)
				}
				for _, c := range []struct {
					what string
					got  filesys.Timestamp
					want *int64
				}{{"created", e.Times.Created, o.Created}, {"modified", e.Times.Modified, o.Modified}, {"changed", e.Times.Changed, o.Changed}, {"accessed", e.Times.Accessed, o.Accessed}} {
					if g := tsUnix(c.got); !sameUnix(g, c.want) || (g != nil && !c.got.ZoneKnown) {
						t.Errorf("%s: %s time %v (zone known %v), oracle %v", p, c.what, derefInt(g), c.got.ZoneKnown, derefInt(c.want))
					}
				}
				if o.Type == "dir" {
					if v := attrOf(e, "valence"); v != itoa(o.Valence) {
						t.Errorf("%s: valence %q, oracle %d", p, v, o.Valence)
					}
					continue
				}
				if e.Size != o.Size {
					t.Errorf("%s: size %d, oracle %d", p, e.Size, o.Size)
				}
				fl, err := f.Open(e)
				if err != nil {
					t.Errorf("%s: Open: %v", p, err)
					continue
				}
				if fl.Size() != o.Size {
					t.Errorf("%s: file size %d, oracle %d", p, fl.Size(), o.Size)
				}
				s := sha256.Sum256(readAll(t, fl))
				if hex.EncodeToString(s[:]) != o.SHA256 {
					t.Errorf("%s: content sha256 differs from the oracle's", p)
				}
				wantRuns := expectedRuns(&ex, o.Size, o.Extents)
				if got := fl.Runs(); !slices.Equal(got, wantRuns) {
					t.Errorf("%s: runs %v, oracle %v", p, first(got, 6), first(wantRuns, 6))
				}
				if err := filesys.CheckRuns(fl.Runs(), o.Size, info.Size); err != nil && o.Size > 0 {
					t.Errorf("%s: %v", p, err)
				}
				live = append(live, fl.Runs()...)
				if o.Type == "symlink" && e.LinkTarget != o.Target {
					t.Errorf("%s: link target %q, oracle %q", p, e.LinkTarget, o.Target)
				}
				if o.HardLink {
					hardlinks[o.InodeID] = append(hardlinks[o.InodeID], p)
					if attrOf(e, "hardlink") != "file" || attrOf(e, "links") != itoa(o.Links) || attrOf(e, "hardlink_inode") != itoa64(o.LinkIno) {
						t.Errorf("%s: hard-link attributes %v, oracle inode %d with %d links", p, e.Attrs, o.LinkIno, o.Links)
					}
				} else if attrOf(e, "hardlink") != "" {
					t.Errorf("%s: flagged as a hard link, the oracle says it is not", p)
				}
			}
			for ino, paths := range hardlinks {
				if o := want[paths[0]]; len(paths) != o.Links {
					t.Errorf("inode %d: %d link paths, oracle link count %d", ino, len(paths), o.Links)
				}
			}
			// The private metadata folder is listed (evidence is not hidden), flagged, with a
			// ~raw~ name because it holds NULs; nothing else carries the flag.
			for p, e := range got {
				if flagged, isPrivate := attrOf(e, "private_metadata") != "", want[p].Private && want[p].Type == "dir"; flagged != isPrivate {
					t.Errorf("%s: private_metadata flag %v, oracle says the private folder is %v", p, flagged, isPrivate)
				}
			}

			// --- free space, exactly, and not over live data
			var wantFree []filesys.Run
			var count int64
			for _, r := range ex.FreeBlocks {
				n := r[1] - r[0] + 1
				count += n
				wantFree = append(wantFree, filesys.Run{Offset: ex.VolumeOffset + r[0]*ex.BlockSize, Length: n * ex.BlockSize})
			}
			if count != ex.FreeCount {
				t.Fatalf("oracle: %d free blocks in its ranges, free_count %d", count, ex.FreeCount)
			}
			free, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			wantRuns(t, free, wantFree)
			var protected []filesys.Run
			protected = append(protected, filesys.Run{Offset: ex.VolumeOffset, Length: 1536})
			for _, sf := range ex.SystemFiles {
				for _, e := range sf.Extents {
					protected = append(protected, filesys.Run{Offset: ex.VolumeOffset + e[0]*ex.BlockSize, Length: e[1] * ex.BlockSize})
				}
			}
			for _, o := range ex.Files {
				for _, e := range o.Extents { // whole allocated blocks, preallocated tails included
					protected = append(protected, filesys.Run{Offset: ex.VolumeOffset + e[0]*ex.BlockSize, Length: e[1] * ex.BlockSize})
				}
			}
			for _, fr := range free {
				for _, lr := range append(slices.Clone(live), protected...) {
					if fr.Offset < lr.Offset+lr.Length && lr.Offset < fr.Offset+fr.Length {
						t.Fatalf("free run %+v overlaps live data %+v", fr, lr)
					}
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a real volume: %v", w)
			}
		})
	}
}

// The populated image holds what the empty ones cannot: it pins the structure
// of the oracle itself, so a regenerated fixture that lost any of these would
// fail here rather than silently weaken the test above.
func TestPopulatedFixtureHoldsWhatItClaims(t *testing.T) {
	ex := loadOracle(t, "hfsplus-populated")
	if !ex.Populated || ex.Catalog.Depth < 2 || ex.ExtentsTree.LeafRecords == 0 || len(ex.Fragments) == 0 {
		t.Fatalf("populated %v, catalog depth %d, extents-overflow records %d, fragmented files %v", ex.Populated, ex.Catalog.Depth, ex.ExtentsTree.LeafRecords, ex.Fragments)
	}
	if ex.Private == nil || len(ex.Private.Inodes) != 1 || ex.Private.Inodes[0].Links != 3 {
		t.Fatalf("private folder %+v", ex.Private)
	}
	kinds := map[string]int{}
	var longest int
	for _, o := range ex.Files {
		kinds[o.Type]++
		longest = max(longest, len([]rune(o.Path[strings.LastIndex(o.Path, "/")+1:])))
	}
	if kinds["symlink"] < 3 || kinds["dir"] < 8 || kinds["file"] < 800 || longest < 85 {
		t.Errorf("kinds %v, longest name %d", kinds, longest)
	}
	// 255 UTF-16 units = the longest ASCII name; the volume stores it as it is.
	img, _ := loadFixture(t, "hfsplus-populated")
	f := open(t, img)
	long := "/long/" + strings.Repeat("n", 251) + ".txt"
	e, err := f.Lookup(long)
	if err != nil || len(e.Name) != 255 {
		t.Fatalf("Lookup of the 255-unit name: %v, %q", err, e.Name)
	}
	// The kernel decomposes names: the volume holds e + U+0301, which the
	// composed spelling still finds (Lookup folds nothing but tries the stored form).
	if e, err := f.Lookup("/café.txt"); err != nil || e.Name != "café.txt" {
		t.Errorf("decomposed name: %v, %q", err, e.Name)
	}
}

// Evidence about the hard-link private folder from a real image (the Linux
// driver names it with four real NULs, as macOS does; hfsplus_oracle.py checks
// the key order with fsck.hfsplus): its key sorts LAST among the root's
// children, after names up to U+65E5. The reader's key comparison must agree
// with the order the real catalog is in, over every record, and must find the
// folder by the B-tree descent alone (no scan of the root's children, which
// would spend directory budget).
func TestRealPrivateFolderSortsLastAndIsFoundByDescent(t *testing.T) {
	img, _ := loadFixture(t, "hfsplus-populated")
	ex := loadOracle(t, "hfsplus-populated")
	if ex.Private == nil || ex.Private.RootIndex != ex.Private.RootKids-1 {
		t.Fatalf("oracle: the private folder is not the root's last child: %+v", ex.Private)
	}
	if !strings.HasPrefix(ex.Private.Name, "\x00\x00\x00\x00HFS+") || ex.RootOrder[len(ex.RootOrder)-1] != ex.Private.Name {
		t.Fatalf("oracle: private folder name %q, root order ends with %q", ex.Private.Name, ex.RootOrder[len(ex.RootOrder)-1])
	}

	f := open(t, img)
	type key struct {
		parent uint32
		name   []uint16
	}
	var prev *key
	var n int
	err := f.CatalogScan(func(parent uint32, name string, _ hfsplus.CatRec) bool {
		k := key{parent, utf16.Encode([]rune(name))} // the key's name (a thread record's data holds another one)
		if prev != nil && hfsplus.CompareKeys(prev.parent, prev.name, k.parent, k.name, false) > 0 {
			t.Errorf("record %d: key (%d, %q) sorts before its predecessor (%d, %q) under the reader's comparison", n, k.parent, name, prev.parent, string(utf16.Decode(prev.name)))
		}
		prev = &k
		n++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 1000 {
		t.Fatalf("scanned %d catalog records", n)
	}

	// The descent finds the folder: opening a hard link costs no directory budget.
	f = open(t, img)
	before := f.DirBudget()
	e, err := f.Lookup("/hl/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fl); len(got) != 23 {
		t.Errorf("hard link content %q", got)
	}
	if after := f.DirBudget(); after != before {
		t.Errorf("resolving a hard link spent %d bytes of directory budget: the private folder was found by a scan, not by the descent", before-after)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func itoa(n int) string     { return itoa64(int64(n)) }
func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func derefInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func first[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func diffStrings(want, got []string) (missing, extra []string) {
	g, w := map[string]bool{}, map[string]bool{}
	for _, s := range got {
		g[s] = true
	}
	for _, s := range want {
		w[s] = true
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	return
}

// displayPath is the path the reader shows for an oracle path: a component
// with a NUL (the private folder) is shown as "~raw~" + base64url of its
// UTF-16 big-endian bytes.
func displayPath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if strings.ContainsRune(c, 0) {
			u := utf16.Encode([]rune(c))
			b := make([]byte, 0, 2*len(u))
			for _, x := range u {
				b = append(b, byte(x>>8), byte(x))
			}
			parts[i] = "~raw~" + base64.RawURLEncoding.EncodeToString(b)
		}
	}
	return strings.Join(parts, "/")
}
