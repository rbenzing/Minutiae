//go:build realimages

package apfs_test

// Manual check of the reader against real APFS images that an examiner supplies
// (the committed fixtures are empty mkapfs volumes; see tools/fixtures/README.md).
// Run it as
//
//	MINUTIAE_TEST_IMAGES=/path go test -tags realimages ./internal/filesys/apfs -run TestRealImages -v
//
// For every <name>.img or <name>.img.gz in $MINUTIAE_TEST_IMAGES/apfs/ that has
// a <name>.expect.json next to it, the image is opened with apfs.Open and:
//
//   - no checksum warning may be raised (every other warning is listed);
//   - Info lists the volumes, with the role and the encryption of each as the
//     JSON says;
//   - every unencrypted volume has exactly the live tree of the JSON (path, type,
//     size, SHA-256 of the content, mode&0o7777, mtime in seconds, symlink
//     target, hard-link groups) and, for each snapshot the JSON names, exactly
//     the tree under /<volume>/.snapshots/<name>/;
//   - an encrypted volume is ErrEncrypted;
//   - when given, the number of unallocated blocks equals "unallocated_blocks";
//   - no Unallocated run overlaps the runs of any file of any tree.
//
// The expected-output JSON (all tree paths are relative to the volume root;
// every field but "volumes" and a volume's "name" is optional, and an omitted
// check is not made):
//
//	{
//	  "image_sha256": "<hex>",                  // sha256 of the (decompressed) image
//	  "unallocated_blocks": 12345,              // blocks Unallocated reports
//	  "volumes": [{
//	    "name": "Test",
//	    "role": "none",                         // as Info's "role=" (none, data, system, ...)
//	    "encrypted": false,
//	    "case_insensitive": true,
//	    "tree": [{
//	      "path": "/a.txt", "type": "file|dir|symlink",
//	      "size": 5, "sha256": "<hex>",          // files only (a symlink: "link")
//	      "mode": 420, "mtime": 1700000000,      // mode&07777 and whole seconds
//	      "link": "target",                      // symlinks
//	      "hardlink_group": "g1"                 // entries with the same non-empty group are one inode
//	    }],
//	    "snapshots": [{"name": "snap1", "tree": [ ...same entries... ]}]
//	  }]
//	}
//
// tools/fixtures/apfs_expect.sh prints a "tree" array for a mounted volume (on
// macOS), for the examiner to paste into the JSON. How to produce an image is
// in tools/fixtures/README.md ("Supplying a real APFS image").

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
)

type realNode struct {
	Path          string `json:"path"`
	Type          string `json:"type"`
	Size          *int64 `json:"size"`
	SHA256        string `json:"sha256"`
	Mode          *int64 `json:"mode"`
	Mtime         *int64 `json:"mtime"`
	Link          string `json:"link"`
	HardlinkGroup string `json:"hardlink_group"`
}

type realVolume struct {
	Name            string     `json:"name"`
	Role            string     `json:"role"`
	Encrypted       bool       `json:"encrypted"`
	CaseInsensitive *bool      `json:"case_insensitive"`
	Tree            []realNode `json:"tree"`
	Snapshots       []struct {
		Name string     `json:"name"`
		Tree []realNode `json:"tree"`
	} `json:"snapshots"`
}

type realExpect struct {
	ImageSHA256       string       `json:"image_sha256"`
	UnallocatedBlocks *int64       `json:"unallocated_blocks"`
	Volumes           []realVolume `json:"volumes"`
}

func TestRealImages(t *testing.T) {
	root := os.Getenv("MINUTIAE_TEST_IMAGES")
	if root == "" {
		t.Skip("MINUTIAE_TEST_IMAGES is not set")
	}
	dir := filepath.Join(root, "apfs")
	var imgs []string
	for _, pat := range []string{"*.img", "*.img.gz"} {
		m, err := filepath.Glob(filepath.Join(dir, pat)) //nolint:gosec // a directory the examiner supplies
		if err != nil {
			t.Fatal(err)
		}
		imgs = append(imgs, m...)
	}
	ran := 0
	for _, p := range imgs {
		name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(p), ".gz"), ".img")
		exp := filepath.Join(dir, name+".expect.json")
		if _, err := os.Stat(exp); err != nil { //nolint:gosec // a path the examiner supplies
			t.Logf("%s: no %s.expect.json, skipped", p, name)
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) { checkRealImage(t, p, exp) })
	}
	if ran == 0 {
		t.Skipf("no <name>.img[.gz] with <name>.expect.json in %s", dir)
	}
}

func readRealImage(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p) //nolint:gosec // an image path the examiner supplies
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(p, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var realVolLine = regexp.MustCompile(`^vol\[\d+\] "(.*)": role=([^,]*), (.*)$`)

func checkRealImage(t *testing.T, imgPath, expPath string) {
	raw, err := os.ReadFile(expPath) //nolint:gosec // a path the examiner supplies
	if err != nil {
		t.Fatal(err)
	}
	var want realExpect
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: %v", expPath, err)
	}
	img := readRealImage(t, imgPath)
	if want.ImageSHA256 != "" {
		if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != want.ImageSHA256 {
			t.Fatalf("image sha256 %x differs from the JSON's %s", sum, want.ImageSHA256)
		}
	}
	fsys, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Volumes: names, role, encryption, case-sensitivity (from Info's feature lines).
	info := fsys.Info()
	if len(info.Volumes) != len(want.Volumes) {
		t.Errorf("Info.Volumes = %q, the JSON lists %d volumes", info.Volumes, len(want.Volumes))
	}
	lines := map[string][]string{} // volume name -> the attributes of its feature line
	for _, f := range info.Features {
		if m := realVolLine.FindStringSubmatch(f); m != nil {
			lines[m[1]] = append([]string{"role=" + m[2]}, strings.Split(m[3], ", ")...)
		}
	}

	var fileRuns []filesys.Run
	for _, v := range want.Volumes {
		t.Run("volume "+v.Name, func(t *testing.T) {
			attrs, ok := lines[v.Name]
			if !ok {
				t.Fatalf("Info lists no volume %q (volumes %q)", v.Name, info.Volumes)
			}
			if v.Role != "" && !slices.Contains(attrs, "role="+v.Role) {
				t.Errorf("volume attributes %q, want role=%s", attrs, v.Role)
			}
			if got := slices.Contains(attrs, "encrypted"); got != v.Encrypted {
				t.Errorf("volume attributes %q, JSON says encrypted=%v", attrs, v.Encrypted)
			}
			if v.CaseInsensitive != nil && slices.Contains(attrs, "case-insensitive") != *v.CaseInsensitive {
				t.Errorf("volume attributes %q, JSON says case_insensitive=%v", attrs, *v.CaseInsensitive)
			}
			if v.Encrypted {
				e, err := fsys.Lookup("/" + v.Name)
				if err == nil {
					_, err = fsys.ReadDir(e)
				}
				if !errors.Is(err, filesys.ErrEncrypted) {
					t.Errorf("encrypted volume: %v, want ErrEncrypted", err)
				}
				return
			}
			fileRuns = append(fileRuns, checkRealTree(t, fsys, "/"+v.Name, "", v.Tree)...)
			for _, s := range v.Snapshots {
				t.Run("snapshot "+s.Name, func(t *testing.T) {
					fileRuns = append(fileRuns, checkRealTree(t, fsys, "/"+v.Name, "/.snapshots/"+s.Name, s.Tree)...)
				})
			}
		})
	}

	runs, err := fsys.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	var free int64
	for _, r := range runs {
		free += r.Length
	}
	if want.UnallocatedBlocks != nil && free/int64(info.BlockSize) != *want.UnallocatedBlocks {
		t.Errorf("Unallocated = %d blocks, the JSON says %d", free/int64(info.BlockSize), *want.UnallocatedBlocks)
	}
	// Sweep: Unallocated returns sorted, merged runs; sort the file runs once.
	data := slices.DeleteFunc(slices.Clone(fileRuns), func(r filesys.Run) bool { return r.Offset < 0 })
	slices.SortFunc(data, func(a, b filesys.Run) int { return cmp.Compare(a.Offset, b.Offset) })
	overlaps, j := 0, 0
	for _, fr := range data {
		for j < len(runs) && runs[j].Offset+runs[j].Length <= fr.Offset {
			j++
		}
		for k := j; k < len(runs) && runs[k].Offset < fr.Offset+fr.Length; k++ {
			if overlaps++; overlaps <= 20 {
				t.Errorf("free run %+v overlaps a file's run %+v", runs[k], fr)
			}
		}
	}
	if overlaps > 20 {
		t.Errorf("%d overlaps in all", overlaps)
	}

	var other []string
	for _, w := range fsys.Info().Warnings {
		if strings.Contains(strings.ToLower(w), "checksum") {
			t.Errorf("checksum warning: %s", w)
		} else {
			other = append(other, w)
		}
	}
	if len(other) != 0 {
		t.Logf("other warnings (%d): %q", len(other), other)
	}
}

// checkRealTree compares the tree below /<vol><sub> (sub is "" for the live tree
// or "/.snapshots/<name>") with want and returns the runs of every file.
func checkRealTree(t *testing.T, fsys *apfs.FS, vol, sub string, want []realNode) []filesys.Run {
	t.Helper()
	rootPath := vol + sub
	rootEntry, err := fsys.Lookup(rootPath)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", rootPath, err)
	}
	got := map[string]filesys.Entry{}
	var runs []filesys.Run
	var walk func(dir filesys.Entry, rel string)
	walk = func(dir filesys.Entry, rel string) {
		es, err := fsys.ReadDir(dir)
		if err != nil {
			t.Errorf("ReadDir(%s%s): %v", rootPath, rel, err)
			return
		}
		for _, e := range es {
			if filesys.IsSnapshotsDir(e) {
				continue
			}
			p := path.Join("/", rel, e.Name)
			got[p] = e
			if e.Type == filesys.TypeDir {
				walk(e, p)
			}
		}
	}
	walk(rootEntry, "")

	groups := map[string]string{} // hardlink_group -> entry ID
	for _, w := range want {
		wp := path.Clean("/" + w.Path)
		e, ok := got[wp]
		if !ok {
			t.Errorf("%s%s: missing", rootPath, wp)
			continue
		}
		delete(got, wp)
		if w.Type != "" && e.Type.String() != w.Type {
			t.Errorf("%s: type %s, want %s", wp, e.Type, w.Type)
		}
		if w.Mode != nil && int64(e.Mode&0o7777) != *w.Mode {
			t.Errorf("%s: mode %o, want %o", wp, e.Mode&0o7777, *w.Mode)
		}
		if w.Mtime != nil && e.Times.Modified.T.Unix() != *w.Mtime {
			t.Errorf("%s: mtime %d, want %d", wp, e.Times.Modified.T.Unix(), *w.Mtime)
		}
		if e.Type == filesys.TypeSymlink && e.LinkTarget != w.Link {
			t.Errorf("%s: link target %q, want %q", wp, e.LinkTarget, w.Link)
		}
		if w.HardlinkGroup != "" {
			if id, seen := groups[w.HardlinkGroup]; seen && id != e.ID {
				t.Errorf("%s: entry id %s, but group %q is %s", wp, e.ID, w.HardlinkGroup, id)
			}
			groups[w.HardlinkGroup] = e.ID
		}
		if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
			continue
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Errorf("%s: Open: %v", wp, err)
			continue
		}
		runs = append(runs, f.Runs()...)
		if e.Type != filesys.TypeFile {
			continue
		}
		if w.Size != nil && f.Size() != *w.Size {
			t.Errorf("%s: size %d, want %d", wp, f.Size(), *w.Size)
		}
		if w.SHA256 != "" {
			h := sha256.New()
			if _, err := io.Copy(h, io.NewSectionReader(f, 0, f.Size())); err != nil {
				t.Errorf("%s: read: %v", wp, err)
			} else if hex.EncodeToString(h.Sum(nil)) != w.SHA256 {
				t.Errorf("%s: content sha256 %x, want %s", wp, h.Sum(nil), w.SHA256)
			}
		}
	}
	// Distinct groups are distinct inodes.
	seen := map[string]string{}
	for g, id := range groups {
		if other, dup := seen[id]; dup {
			t.Errorf("hard-link groups %q and %q are the same inode (%s)", g, other, id)
		}
		seen[id] = g
	}
	for p := range got {
		t.Errorf("%s%s: present in the image but not in the JSON", rootPath, p)
	}
	return runs
}
