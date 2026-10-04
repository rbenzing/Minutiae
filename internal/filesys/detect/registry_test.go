package detect_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// gunzipFixture returns the decompressed image of a committed fixture in
// another driver's testdata directory.
func gunzipFixture(t *testing.T, rel string) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.FromSlash(rel))
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
	return img
}

// The probe order is the spec's (apfs, f2fs, ext4, exfat, hfsplus, fat) over
// the drivers that exist; and every driver is probed against every image, builder
// made or a committed real fixture, so exactly one driver claims each of them
// whatever the order (an overlap would be a driver stealing another's images).
func TestDriversClaimExactlyTheirOwnImages(t *testing.T) {
	var names []string
	for _, d := range detect.Drivers {
		names = append(names, d.Name)
	}
	if want := []string{"apfs", "f2fs", "ext4", "exfat", "hfsplus", "fat", "f2fs-backup"}; !slices.Equal(names, want) {
		t.Fatalf("driver order = %v, want %v", names, want)
	}

	type image struct {
		img  []byte
		load func(t *testing.T) []byte // when set, builds the image on demand with the subtest's t (the real APFS fixtures are 64 MiB and more)
		want string                    // detect driver name
		typ  string                    // Info().Type
	}
	apfsVolume := apfstest.Volume{Name: "V", Files: []apfstest.File{{Path: "/a", Data: []byte("a")}}}
	images := map[string]image{
		"builder f2fs": {img: f2fstest.Build(f2fstest.Options{Segments: 2, Label: "data"}, []f2fstest.File{{Path: "/a.txt", Data: []byte("a"), Inline: true}}), want: "f2fs", typ: "f2fs"},
		"builder f2fs with a blank second checkpoint": {img: f2fstest.Build(f2fstest.Options{Segments: 1, NoPack2: true}, nil), want: "f2fs", typ: "f2fs"},

		"builder ext4":              {img: ext4test.Build(ext4test.Options{Extents: true}, nil), want: "ext4", typ: "ext4"},
		"builder exfat":             {img: exfattest.Build(exfattest.Options{}, nil), want: "exfat", typ: "exfat"},
		"builder fat12":             {img: fattest.Build(fattest.Options{Type: 12}, nil), want: "fat", typ: "fat12"},
		"builder fat16":             {img: fattest.Build(fattest.Options{Type: 16}, nil), want: "fat", typ: "fat16"},
		"builder fat32":             {img: fattest.Build(fattest.Options{Type: 32}, nil), want: "fat", typ: "fat32"},
		"builder apfs":              {img: apfstest.Build(apfstest.Options{Blocks: 2048, Volumes: []apfstest.Volume{apfsVolume}}), want: "apfs", typ: "apfs"},
		"builder hfsplus":           {img: hfsplustest.Build(hfsplustest.Options{Label: "H"}, nil), want: "hfsplus", typ: "hfsplus"},
		"builder hfsplus journaled": {img: hfsplustest.Build(hfsplustest.Options{Label: "H", Journaled: true}, nil), want: "hfsplus", typ: "hfsplus"},
		"builder hfsx":              {img: hfsplustest.Build(hfsplustest.Options{Label: "H", HFSX: true, CaseSensitive: true}, nil), want: "hfsplus", typ: "hfsx"},
		"builder hfsplus wrapped":   {img: hfsplustest.Build(hfsplustest.Options{Label: "H", Wrapper: true}, nil), want: "hfsplus", typ: "hfsplus"},
		"builder hfsx wrapped":      {img: hfsplustest.Build(hfsplustest.Options{Label: "H", HFSX: true, Wrapper: true}, nil), want: "hfsplus", typ: "hfsx"},

		"builder apfs, two checkpoints, CAB layer": {img: apfstest.Build(apfstest.Options{Blocks: 2048, Checkpoints: 2, ChunksPerCIB: 1, CibsPerCAB: 1}), want: "apfs", typ: "apfs"},
		"builder apfs, 64 KiB blocks":              {img: apfstest.Build(apfstest.Options{BlockSize: 65536, Blocks: 256}), want: "apfs", typ: "apfs"},
	}
	for name, file := range map[string]string{
		"real apfs ci": "apfs-ci", "real apfs cs": "apfs-cs", "real apfs multichunk": "apfs-multichunk",
	} {
		path := "../apfs/testdata/" + file + ".img.gz"
		images[name] = image{load: func(t *testing.T) []byte { return gunzipFixture(t, path) }, want: "apfs", typ: "apfs"}
	}
	for name, f := range map[string]struct{ path, want, typ string }{
		"real ext4 4k":         {"../ext4/testdata/ext4-4k-csum.img.gz", "ext4", "ext4"},
		"real fat12":           {"../fat/testdata/fat12.img.gz", "fat", "fat12"},
		"real fat16":           {"../fat/testdata/fat16.img.gz", "fat", "fat16"},
		"real fat32":           {"../fat/testdata/fat32.img.gz", "fat", "fat32"},
		"real exfat":           {"../exfat/testdata/exfat.img.gz", "exfat", "exfat"},
		"real f2fs":            {"../f2fs/testdata/f2fs-default.img.gz", "f2fs", "f2fs"},
		"real hfsplus":         {"../hfsplus/testdata/hfsplus-empty.img.gz", "hfsplus", "hfsplus"},
		"real hfsplus 1k":      {"../hfsplus/testdata/hfsplus-1k.img.gz", "hfsplus", "hfsplus"},
		"real hfsplus journal": {"../hfsplus/testdata/hfsplus-journal.img.gz", "hfsplus", "hfsplus"},
		"real hfsplus wrapped": {"../hfsplus/testdata/hfsplus-wrapped.img.gz", "hfsplus", "hfsplus"},
		"real hfsx":            {"../hfsplus/testdata/hfsx-empty.img.gz", "hfsplus", "hfsx"},
		"real f2fs extra attr": {"../f2fs/testdata/f2fs-extra-attr.img.gz", "f2fs", "f2fs"},
	} {
		images[name] = image{img: gunzipFixture(t, f.path), want: f.want, typ: f.typ}
	}

	for name, tc := range images {
		t.Run(name, func(t *testing.T) {
			if tc.load != nil {
				if testing.Short() && strings.HasSuffix(name, "multichunk") {
					t.Skip("large fixture skipped under -short")
				}
				tc.img = tc.load(t)
			}
			r := bytes.NewReader(tc.img)
			size := int64(len(tc.img))

			var claimed []string
			for _, d := range detect.Drivers {
				if d.Probe(r, size) {
					claimed = append(claimed, d.Name)
				}
			}
			if !slices.Equal(claimed, []string{tc.want}) {
				t.Fatalf("drivers claiming the image = %v, want exactly [%s]", claimed, tc.want)
			}

			if got, ok := detect.Probe(r, size); !ok || got != tc.want {
				t.Fatalf("Probe = %q, %v; want %q", got, ok, tc.want)
			}
			fsys, err := detect.Open(r, size)
			if err != nil {
				t.Fatal(err)
			}
			if got := fsys.Info().Type; got != tc.typ {
				t.Errorf("Info().Type = %q, want %q", got, tc.typ)
			}
			if w := fsys.Info().Warnings; slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "ambiguous signatures") }) {
				t.Errorf("a clean image carries an ambiguity note: %q", w)
			}
		})
	}
}

// A driver that claims an image but cannot open it returns an untyped nil
// FileSystem with the error, never a typed nil pointer in the interface.
func TestRealDriversOpenReturnsUntypedNilOnError(t *testing.T) {
	// Boot sectors that pass Probe but fail Open are hard to build for the
	// size-checked drivers, so call Open with an image too short for any of them.
	short := make([]byte, 100)
	for _, d := range detect.Drivers {
		fsys, err := d.Open(bytes.NewReader(short), int64(len(short)))
		if err == nil || fsys != nil {
			t.Errorf("%s: Open(short image) = %v, %v; want an untyped nil and an error", d.Name, fsys, err)
		}
	}
}

// plantF2FSBackup writes the head of an F2FS superblock (magic F2F52010,
// major_ver 1, log_blocksize 12) where the backup copy lives, byte 4096+1024.
// It models a stale signature left by a previous format, or a forged one.
func plantF2FSBackup(img []byte) []byte {
	out := bytes.Clone(img)
	const backup = 4096 + 1024
	binary.LittleEndian.PutUint32(out[backup:], 0xF2F52010)
	binary.LittleEndian.PutUint16(out[backup+4:], 1)
	binary.LittleEndian.PutUint32(out[backup+16:], 12)
	return out
}

// A stale or forged F2FS backup superblock inside another filesystem must not
// hide it: f2fs matches only a valid PRIMARY superblock, and the last-resort
// f2fs-backup driver comes after every other driver, so each real image still
// probes and opens as its own type.
func TestStaleF2FSBackupSuperblockDoesNotHideOtherFilesystems(t *testing.T) {
	backupDriver := slices.IndexFunc(detect.Drivers, func(d detect.Driver) bool { return d.Name == "f2fs-backup" })
	if backupDriver < 0 {
		t.Fatal("no f2fs-backup driver is registered")
	}
	for name, path := range map[string]string{
		"ext4 4k": "../ext4/testdata/ext4-4k-csum.img.gz",
		"ext2 1k": "../ext4/testdata/ext4-1k-blockmap-ext2.img.gz",
		"fat12":   "../fat/testdata/fat12.img.gz",
		"fat16":   "../fat/testdata/fat16.img.gz",
		"fat32":   "../fat/testdata/fat32.img.gz",
		"exfat":   "../exfat/testdata/exfat.img.gz",
	} {
		t.Run(name, func(t *testing.T) {
			pristine := gunzipFixture(t, path)
			wantName, ok := detect.Probe(bytes.NewReader(pristine), int64(len(pristine)))
			if !ok || wantName == "f2fs" || wantName == "f2fs-backup" {
				t.Fatalf("pristine Probe = %q, %v", wantName, ok)
			}
			base, err := detect.Open(bytes.NewReader(pristine), int64(len(pristine)))
			if err != nil {
				t.Fatal(err)
			}
			wantType := base.Info().Type

			img := plantF2FSBackup(pristine)
			size := int64(len(img))
			r := bytes.NewReader(img)
			if !detect.Drivers[backupDriver].Probe(r, size) {
				t.Fatal("the planted backup superblock is not seen by f2fs-backup: the test plants nothing")
			}
			for _, d := range detect.Drivers {
				if d.Name == "f2fs" && d.Probe(r, size) {
					t.Fatal("f2fs claims an image with only a backup signature")
				}
			}
			if got, ok := detect.Probe(r, size); !ok || got != wantName {
				t.Errorf("Probe = %q, %v; want %q", got, ok, wantName)
			}
			fsys, err := detect.Open(r, size)
			if err != nil {
				t.Fatal(err)
			}
			if got := fsys.Info().Type; got != wantType {
				t.Errorf("Info().Type = %q, want %q", got, wantType)
			}
		})
	}
}

// An F2FS image whose primary superblock is destroyed (a valid backup remains)
// is claimed only by the last-resort f2fs-backup driver and opens as "f2fs"
// through the backup, with the backup-in-use warning.
func TestF2FSDestroyedPrimaryOpensThroughBackupDriver(t *testing.T) {
	img := gunzipFixture(t, "../f2fs/testdata/f2fs-default.img.gz")
	clear(img[1024:4096])
	size := int64(len(img))
	r := bytes.NewReader(img)

	var claimed []string
	for _, d := range detect.Drivers {
		if d.Probe(r, size) {
			claimed = append(claimed, d.Name)
		}
	}
	if !slices.Equal(claimed, []string{"f2fs-backup"}) {
		t.Fatalf("drivers claiming the image = %v, want exactly [f2fs-backup]", claimed)
	}
	if got, ok := detect.Probe(r, size); !ok || got != "f2fs-backup" {
		t.Fatalf("Probe = %q, %v; want f2fs-backup", got, ok)
	}
	fsys, err := detect.Open(r, size)
	if err != nil {
		t.Fatal(err)
	}
	info := fsys.Info()
	if info.Type != "f2fs" {
		t.Errorf("Info().Type = %q, want f2fs (the driver name is only for detection)", info.Type)
	}
	if !slices.ContainsFunc(info.Warnings, func(w string) bool { return strings.Contains(w, "backup superblock") }) {
		t.Errorf("Warnings = %q, want one saying the backup superblock is in use", info.Warnings)
	}
}

// plantNXSB copies the first n bytes of a (stale) APFS container superblock
// block into img at off and returns the modified copy.
func plantNXSB(img []byte, off, n int) []byte {
	apfsImg := apfstest.Build(apfstest.Options{Blocks: 2048, Volumes: []apfstest.Volume{{Name: "V"}}})
	out := bytes.Clone(img)
	copy(out[off:], apfsImg[:n]) // block 0 of an APFS container: the superblock
	return out
}

// An APFS container superblock (NXSB) planted inside another filesystem's image
// must not hide it. The APFS probe looks at block 0 only, so a stale or forged
// copy anywhere else (a leftover from an earlier format, a forged backup) changes
// nothing: each image still probes and opens as its own type, without a warning.
// The one place the probe does look, the first block, is shared with the other
// filesystems' boot areas: where a filesystem leaves those free (ext4 and F2FS
// keep their superblock at byte 1024) a signature planted there is claimed by
// the apfs driver first, whose Open fails on it with a corrupt-structure error,
// so detection falls through to the real driver and records the failure as a
// warning (it still opens as its own type).
func TestStaleAPFSSuperblockDoesNotHideOtherFilesystems(t *testing.T) {
	apfsIdx := slices.IndexFunc(detect.Drivers, func(d detect.Driver) bool { return d.Name == "apfs" })
	if apfsIdx < 0 {
		t.Fatal("no apfs driver is registered")
	}
	for name, path := range map[string]string{
		"ext4 4k":    "../ext4/testdata/ext4-4k-csum.img.gz",
		"ext2 1k":    "../ext4/testdata/ext4-1k-blockmap-ext2.img.gz",
		"fat12":      "../fat/testdata/fat12.img.gz",
		"fat16":      "../fat/testdata/fat16.img.gz",
		"fat32":      "../fat/testdata/fat32.img.gz",
		"exfat":      "../exfat/testdata/exfat.img.gz",
		"f2fs":       "../f2fs/testdata/f2fs-default.img.gz",
		"f2fs extra": "../f2fs/testdata/f2fs-extra-attr.img.gz",
		"hfsplus":    "../hfsplus/testdata/hfsplus-empty.img.gz",
		"hfsx":       "../hfsplus/testdata/hfsx-empty.img.gz",
	} {
		t.Run(name, func(t *testing.T) {
			pristine := gunzipFixture(t, path)
			wantName, ok := detect.Probe(bytes.NewReader(pristine), int64(len(pristine)))
			if !ok || wantName == "apfs" {
				t.Fatalf("pristine Probe = %q, %v", wantName, ok)
			}
			base, err := detect.Open(bytes.NewReader(pristine), int64(len(pristine)))
			if err != nil {
				t.Fatal(err)
			}
			wantType := base.Info().Type

			// Planted past block 0 in unused (all-zero) space, aligned and not, the way
			// a leftover of an earlier format sits: nothing changes.
			offsets := zeroOffsets(pristine, 3)
			if len(offsets) < 3 {
				t.Fatalf("the fixture has %d unused areas to plant in, want 3 (aligned and unaligned)", len(offsets))
			}
			for _, off := range offsets {
				img := plantNXSB(pristine, off, 4096)
				r, size := bytes.NewReader(img), int64(len(img))
				if detect.Drivers[apfsIdx].Probe(r, size) {
					t.Fatalf("offset %d: the apfs probe claims a signature that is not at block 0", off)
				}
				if got, ok := detect.Probe(r, size); !ok || got != wantName {
					t.Errorf("offset %d: Probe = %q, %v; want %q", off, got, ok, wantName)
				}
				fsys, err := detect.Open(r, size)
				if err != nil {
					t.Fatalf("offset %d: %v", off, err)
				}
				if got := fsys.Info().Type; got != wantType {
					t.Errorf("offset %d: Info().Type = %q, want %q", off, got, wantType)
				}
				for _, w := range fsys.Info().Warnings {
					if strings.Contains(w, "driver apfs") {
						t.Errorf("offset %d: warning %q", off, w)
					}
				}
			}
		})
	}

	// Block 0 itself, over a free boot area (only the first KiB of the APFS
	// block: the filesystem's own superblock stays intact).
	for name, path := range map[string]string{
		"ext4 4k": "../ext4/testdata/ext4-4k-csum.img.gz",
		"f2fs":    "../f2fs/testdata/f2fs-default.img.gz",
		"hfsplus": "../hfsplus/testdata/hfsplus-empty.img.gz",
		"hfsx":    "../hfsplus/testdata/hfsx-empty.img.gz",
	} {
		t.Run("block 0 over the boot area of "+name, func(t *testing.T) {
			pristine := gunzipFixture(t, path)
			base, err := detect.Open(bytes.NewReader(pristine), int64(len(pristine)))
			if err != nil {
				t.Fatal(err)
			}
			img := plantNXSB(pristine, 0, 1024)
			r, size := bytes.NewReader(img), int64(len(img))
			if !detect.Drivers[apfsIdx].Probe(r, size) {
				t.Fatal("the planted signature is not seen by the apfs probe: the test plants nothing")
			}
			fsys, err := detect.Open(r, size)
			if err != nil {
				t.Fatalf("Open: %v (a stale signature must not make the real filesystem unreadable)", err)
			}
			info := fsys.Info()
			if info.Type != base.Info().Type {
				t.Errorf("Info().Type = %q, want %q", info.Type, base.Info().Type)
			}
			if !slices.ContainsFunc(info.Warnings, func(w string) bool {
				return strings.Contains(w, "driver apfs matched but failed to open") && strings.Contains(w, "opened as")
			}) {
				t.Errorf("Warnings = %q, want the apfs failure noted", info.Warnings)
			}
		})
	}
}

// zeroOffsets returns up to n offsets past the first 16 KiB where img has
// 4096 + 512 bytes of zeros: the first aligned to 4096, the others spread out
// and 512 bytes off alignment.
func zeroOffsets(img []byte, n int) []int {
	zero := func(off int) bool {
		return off+4096+512 <= len(img) && !slices.ContainsFunc(img[off:off+4096+512], func(b byte) bool { return b != 0 })
	}
	var out []int
	for off := 16384; off < len(img) && len(out) < n; off += 4096 {
		if zero(off) {
			out = append(out, off)
			if len(out) == 1 {
				off += 64 << 10
			}
			off += 512 // later hits are not 4096-aligned
		}
	}
	return out
}

// An HFS+, HFSX and wrapped HFS+ image is detected as the hfsplus driver and
// opens with Info().Type hfsplus or hfsx; the images of every other driver are
// still detected as themselves (TestDriversClaimExactlyTheirOwnImages claims
// each image by exactly one driver), and none of them opens as HFS+.
func TestDetectOpensHFSPlus(t *testing.T) {
	for name, tc := range map[string]struct {
		img  []byte
		typ  string
		wrap bool
	}{
		"hfsplus": {hfsplustest.Build(hfsplustest.Options{Label: "H"}, nil), "hfsplus", false},
		"hfsx":    {hfsplustest.Build(hfsplustest.Options{Label: "H", HFSX: true, CaseSensitive: true}, nil), "hfsx", false},
		"wrapped": {hfsplustest.Build(hfsplustest.Options{Label: "H", Wrapper: true}, nil), "hfsplus", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(tc.img)
			size := int64(len(tc.img))
			if got, ok := detect.Probe(r, size); !ok || got != "hfsplus" {
				t.Fatalf("Probe = %q, %v; want hfsplus", got, ok)
			}
			fsys, err := detect.Open(r, size)
			if err != nil {
				t.Fatal(err)
			}
			info := fsys.Info()
			if info.Type != tc.typ || info.Label != "H" {
				t.Errorf("Info = %+v, want type %s label H", info, tc.typ)
			}
			if tc.wrap && !slices.Contains(info.Features, "hfs-wrapper") {
				t.Errorf("Features = %v, want hfs-wrapper", info.Features)
			}
			if _, err := fsys.Unallocated(); err != nil {
				t.Errorf("Unallocated: %v", err)
			}
		})
	}
	for name, tc := range map[string]struct {
		img    []byte
		driver string
	}{
		"ext4":  {ext4test.Build(ext4test.Options{Extents: true}, nil), "ext4"},
		"f2fs":  {f2fstest.Build(f2fstest.Options{Segments: 2}, nil), "f2fs"},
		"exfat": {exfattest.Build(exfattest.Options{}, nil), "exfat"},
		"fat32": {fattest.Build(fattest.Options{Type: 32}, nil), "fat"},
	} {
		img := tc.img
		if got, ok := detect.Probe(bytes.NewReader(img), int64(len(img))); !ok || got != tc.driver {
			t.Errorf("%s image: Probe = %q, %v; want %s", name, got, ok, tc.driver)
		}
		fsys, err := detect.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if typ := fsys.Info().Type; typ == "hfsplus" || typ == "hfsx" {
			t.Errorf("%s image opened as %s", name, typ)
		}
	}
}
