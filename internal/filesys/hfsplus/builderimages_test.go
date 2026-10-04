package hfsplus_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// builderImage is one populated image of the synthetic builder.
type builderImage struct {
	name string
	img  []byte
}

// builderImages returns the builder's populated images, one per on-disk
// feature the real fixtures cannot hold: a multi-level catalog, HFSX, hard
// links with the private folder, fragmented forks with extents-overflow
// records (also for the catalog file), the attributes tree, a resource fork,
// compressed files, a wrapper and a journal. They seed the fuzz target and are
// what TestBuilderImagesPassFsck hands to Apple's fsck.
func builderImages() []builderImage {
	data := pattern(9000, 3)
	var many []hfsplustest.File
	many = append(many, hfsplustest.File{Path: "/many", Dir: true})
	for i := range 200 {
		many = append(many, hfsplustest.File{Path: fmt.Sprintf("/many/entry-%03d.txt", i), Data: []byte{byte(i)}})
	}
	plain := []hfsplustest.File{
		{Path: "/a.txt", Data: []byte("hello")},
		{Path: "/dir", Dir: true},
		{Path: "/dir/Mixed.TXT", Data: data},
		{Path: "/empty", Data: nil},
		{Path: "/link", Mode: 0o120777, Data: []byte("dir/Mixed.TXT")},
	}
	tree := func(extra ...hfsplustest.File) []hfsplustest.File { return append(slicesClone(plain), extra...) }
	return []builderImage{
		{"plain", hfsplustest.Build(hfsplustest.Options{Label: "PLAIN"}, plain)},
		{"multilevel", hfsplustest.Build(hfsplustest.Options{Label: "MANY", Blocks: 512, NodeSize: 512}, many)},
		{"hfsx", hfsplustest.Build(hfsplustest.Options{Label: "HFSX", HFSX: true, CaseSensitive: true}, plain)},
		{"hfsx-folding", hfsplustest.Build(hfsplustest.Options{Label: "HFSXF", HFSX: true}, plain)},
		{"hardlinks", hfsplustest.Build(hfsplustest.Options{Label: "LINKS"}, rootOwnedLinkTree())},
		{"hardlinks-owned", hfsplustest.Build(hfsplustest.Options{Label: "LINKS"}, linkTree())},
		{"overflow-extents", hfsplustest.Build(hfsplustest.Options{Label: "OVER", Blocks: 512}, tree(
			hfsplustest.File{Path: "/frag.bin", Data: pattern(30*4096, 5), Fragment: 1},
			hfsplustest.File{Path: "/frag2.bin", Data: pattern(12*4096, 6), Fragment: 2}))},
		{"fragmented-catalog", hfsplustest.Build(hfsplustest.Options{Label: "CATF", Blocks: 512, NodeSize: 512, CatalogFragment: 2}, many)},
		{"attributes", hfsplustest.Build(hfsplustest.Options{Label: "ATTR"}, tree(
			hfsplustest.File{Path: "/x.txt", Data: []byte("x"), Attrs: []hfsplustest.Attr{
				{Name: "com.apple.FinderInfo", Value: make([]byte, 32)},
				{Name: "user.note", Value: []byte("a note")},
			}}))},
		{"resource-fork", hfsplustest.Build(hfsplustest.Options{Label: "RSRC"}, tree(
			hfsplustest.File{Path: "/res", Data: []byte("data"), Rsrc: pattern(5000, 8)}))},
		{"compressed", hfsplustest.Build(hfsplustest.Options{Label: "CMP"}, tree(
			hfsplustest.File{Path: "/z3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 5000, zlibBytes(bytes.Repeat([]byte("z"), 5000)))}}},
			hfsplustest.File{Path: "/lzvn", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(7, 1234, []byte("xxxx"))}}}))},
		{"wrapped", hfsplustest.Build(hfsplustest.Options{Label: "WRAP", Wrapper: true}, plain)},
		{"wrapped-hfsx", hfsplustest.Build(hfsplustest.Options{Label: "WRAPX", Wrapper: true, HFSX: true, CaseSensitive: true}, plain)},
		{"journaled-clean", hfsplustest.Build(hfsplustest.Options{Label: "JRNL", Journaled: true}, plain)},
		{"blocks-1k", hfsplustest.Build(hfsplustest.Options{Label: "ONEK", BlockSize: 1024, Blocks: 1024}, plain)},
	}
}

// rootOwnedLinkTree is linkTree with every owner and group 0: Apple's fsck accepts
// the builder's hard links only then (see fsckExempt).
func rootOwnedLinkTree() []hfsplustest.File {
	files := linkTree()
	for i := range files {
		files[i].UID, files[i].GID = 0, 0
	}
	return files
}

// fsckExempt are the builder images Apple's fsck.hfsplus does not accept, with
// the reason; the reader opens them on purpose. They are written to the
// exempt/ subdirectory (not checked) with the reasons in REASONS.txt.
var fsckExempt = map[string]string{
	"hardlinks-owned": "fsck.hfsplus reports \"filelink prime buckets do not match\" (Incorrect number of file hard links) for a hard-link group whose files have a non-zero uid or gid; with uid = gid = 0 it passes (the builder's \"hardlinks\" image, and the Linux-written real fixture). The builder writes no link chain (prev/next link ids, kHFSHasLinkChainMask), which the Linux driver does not write either; the cause of the owner dependence is unconfirmed. The reader's tests of per-link ownership need this image.",
	"journaled-clean": "the builder lays out the journal info block and the journal but adds no .journal_info_block and .journal catalog files, so fsck.hfsplus calls the journal blocks orphaned (bitmap repair, free count 245 instead of 236). The reader takes the journal from the volume header and the journal info block and needs no catalog entry; the real journaled fixture (mkfs.hfsplus -J) has the files.",
	"wrapped-hfsx":    "fsck.hfsplus rejects an HFSX volume inside an HFS wrapper (Invalid volume header): Apple never writes one, the wrapper's embedded signature must be H+. The reader accepts H+ and HX there on purpose.",
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

// The builder images must be images the reader accepts, or the fuzz starts
// from nothing and the fsck report below would test nothing.
func TestFuzzBuilderSeedsOpen(t *testing.T) {
	for _, b := range builderImages() {
		f, err := hfsplus.Open(bytes.NewReader(b.img), int64(len(b.img)))
		if err != nil {
			t.Errorf("%s: %v", b.name, err)
			continue
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("%s: warnings %v", b.name, w)
		}
	}
}

// TestBuilderImagesPassFsck is the generator-time report that the builder's
// images are in the format Apple's checker accepts (it is not part of go test:
// it needs the directory in MINUTIAE_WRITE_BUILDER_IMAGES). It writes every
// builder image there as <name>.img; then run
//
//	docker run --rm -v "$PWD:/work" -v <dir>:/img -w /work minutiae-fixtures-hfs bash tools/fixtures/hfsplus.sh check-builder /img
//
// which runs fsck.hfsplus -n -f on each one. A failure is a builder defect.
func TestBuilderImagesPassFsck(t *testing.T) {
	dir := os.Getenv("MINUTIAE_WRITE_BUILDER_IMAGES")
	if dir == "" {
		t.Skip("set MINUTIAE_WRITE_BUILDER_IMAGES=<dir> to write the builder images for hfsplus.sh check-builder")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // test helper: the output directory is the examiner's own choice
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "exempt"), 0o750); err != nil { //nolint:gosec // as above
		t.Fatal(err)
	}
	var reasons strings.Builder
	for _, b := range builderImages() {
		p := filepath.Join(dir, b.name+".img")
		if why, ok := fsckExempt[b.name]; ok {
			p = filepath.Join(dir, "exempt", b.name+".img")
			fmt.Fprintf(&reasons, "%s: %s\n\n", b.name, why)
		}
		if err := os.WriteFile(p, b.img, 0o600); err != nil { //nolint:gosec // as above
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exempt", "REASONS.txt"), []byte(reasons.String()), 0o600); err != nil { //nolint:gosec // as above
		t.Fatal(err)
	}
}

// Two builder defects found by Apple's fsck (TestBuilderImagesPassFsck): a file
// with extended attributes carries kHFSHasAttributesMask in its catalog flags,
// and a hard-link record's createDate is the private folder's (FixedDate),
// whatever dates the file asked for.
func TestBuilderMatchesFsckExpectations(t *testing.T) {
	files := append(linkTree(), hfsplustest.File{
		Path: "/attr.txt", Data: []byte("a"),
		Attrs: []hfsplustest.Attr{{Name: "user.k", Value: []byte("v")}},
	})
	_, _, f := buildTree(t, hfsplustest.Options{}, files)
	rec, _, err := f.FindRecord(2, "attr.txt")
	if err != nil || rec.Flags&0x04 == 0 {
		t.Errorf("attr.txt: flags %#x, %v; want kHFSHasAttributesMask (0x04)", rec.Flags, err)
	}
	if rec, _, err := f.FindRecord(2, "plain.txt"); err == nil && rec.Flags&0x04 != 0 {
		t.Errorf("plain.txt carries the attributes flag")
	}
	for _, name := range []string{"/b/second.txt", "/a/first.txt"} {
		base, cn := name[3:], map[string]uint32{"/a": 16, "/b": 17}[name[:2]]
		rec, _, err := f.FindRecord(cn, base)
		if err != nil || rec.Create != hfsplustest.FixedDate {
			t.Errorf("%s: link createDate %d, %v; want FixedDate %d", name, rec.Create, err, hfsplustest.FixedDate)
		}
	}
}
