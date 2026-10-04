package examine_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// TestExtractFromF2FSFixture imports each real mkfs.f2fs/sload.f2fs image and
// extracts it through the default driver registry: hashes, sizes and times
// equal the oracle computed from the source tree (symlinks are extracted as
// their target text), Derived.ParentSHA256 is the imported image's, FSType is
// "f2fs", there are no filesystem warnings, the unallocated export is exactly
// the free blocks that dump.f2fs reports, and case verify is OK.
func TestExtractFromF2FSFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts two 64 MiB images (about 625 artifacts each)")
	}
	for _, name := range []string{"f2fs-extra-attr", "f2fs-default"} {
		t.Run(name, func(t *testing.T) {
			extractFixture(t, "f2fs", name, "f2fs", true, true)
		})
	}
}

// Names of 255 bytes, the most F2FS stores, extract without aborting the run:
// the local path component is shortened (with a hash suffix) while the
// provenance keeps the original path.
func TestExtractF2FSLongNames(t *testing.T) {
	ascii := strings.Repeat("a", 251) + ".txt"
	cjk := strings.Repeat("日", 85) // 255 bytes
	dir := strings.Repeat("d", 255)
	img := f2fstest.Build(f2fstest.Options{Segments: 1}, []f2fstest.File{
		{Path: "/" + ascii, Data: []byte("one"), Inline: true},
		{Path: "/" + cjk, Data: pattern(5000, 1)},
		{Path: "/" + dir, Dir: true},
		{Path: "/" + dir + "/" + ascii, Data: []byte("four"), Inline: true},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 3 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Artifacts) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	want := map[string][]byte{
		"/" + ascii:             []byte("one"),
		"/" + cjk:               pattern(5000, 1),
		"/" + dir + "/" + ascii: []byte("four"),
	}
	locals := map[string]string{}
	for _, r := range sum.Artifacts {
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 {
				t.Errorf("component of %d bytes in %q", len(comp), r.Path)
			}
		}
		w, ok := want[r.Source.RemotePath]
		if !ok || string(readArtifact(t, c, r)) != string(w) {
			t.Errorf("artifact %q (remote %q) content wrong", r.Path, r.Source.RemotePath)
		}
		if r.Source.Derived == nil || r.Source.Derived.FSPath != r.Source.RemotePath || r.Source.Derived.FSType != "f2fs" {
			t.Errorf("provenance lost the original path: %+v", r.Source)
		}
		if prev, dup := locals[r.Path]; dup {
			t.Errorf("artifacts %q and %q share the local path %q", prev, r.Source.RemotePath, r.Path)
		}
		locals[r.Path] = r.Source.RemotePath
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}

// Empty files and files stored inline in the inode have no data blocks: they
// are extracted whole as artifacts with nil runs (there is no image range to
// record), without any warning, while a file with data blocks records its runs.
func TestExtractF2FSEmptyAndInlineFiles(t *testing.T) {
	img := f2fstest.Build(f2fstest.Options{Segments: 1, ExtraAttr: true, InodeChksum: true}, []f2fstest.File{
		{Path: "/EMPTY.TXT"},
		{Path: "/INLINE.TXT", Data: []byte("inline"), Inline: true},
		{Path: "/INLINK", Symlink: "INLINE.TXT", Inline: true},
		{Path: "/FULL.BIN", Data: pattern(6000, 2)},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 4 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Warnings) != 0 || len(sum.Artifacts) != 4 {
		t.Fatalf("summary = %+v", sum)
	}
	seen := 0
	for _, rec := range sum.Artifacts {
		d := rec.Source.Derived
		if d == nil || rec.Incomplete {
			t.Errorf("%s: derived %+v incomplete %v", rec.Source.RemotePath, d, rec.Incomplete)
			continue
		}
		seen++
		got := readArtifact(t, c, rec)
		switch rec.Source.RemotePath {
		case "/EMPTY.TXT":
			if rec.Size != 0 || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("empty file: size %d derived %+v, want an empty artifact with nil runs", rec.Size, d)
			}
		case "/INLINE.TXT":
			if string(got) != "inline" || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("inline file: %q derived %+v, want its text and nil runs", got, d)
			}
		case "/INLINK":
			if string(got) != "INLINE.TXT" || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("inline symlink: %q derived %+v, want its target and nil runs", got, d)
			}
		case "/FULL.BIN":
			if string(got) != string(pattern(6000, 2)) || len(d.Runs) == 0 {
				t.Errorf("file with data blocks: size %d runs %v", rec.Size, d.Runs)
			}
		default:
			seen--
			t.Errorf("unexpected artifact %q", rec.Source.RemotePath)
		}
	}
	if seen != 4 {
		t.Errorf("checked %d of 4 artifacts", seen)
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}
