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

	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
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
	if want := []string{"f2fs", "ext4", "exfat", "fat", "f2fs-backup"}; !slices.Equal(names, want) {
		t.Fatalf("driver order = %v, want %v", names, want)
	}

	type image struct {
		img  []byte
		want string // detect driver name
		typ  string // Info().Type
	}
	images := map[string]image{
		"builder f2fs": {f2fstest.Build(f2fstest.Options{Segments: 2, Label: "data"}, []f2fstest.File{{Path: "/a.txt", Data: []byte("a"), Inline: true}}), "f2fs", "f2fs"},
		"builder f2fs with a blank second checkpoint": {f2fstest.Build(f2fstest.Options{Segments: 1, NoPack2: true}, nil), "f2fs", "f2fs"},
		"builder ext4":  {ext4test.Build(ext4test.Options{Extents: true}, nil), "ext4", "ext4"},
		"builder exfat": {exfattest.Build(exfattest.Options{}, nil), "exfat", "exfat"},
		"builder fat12": {fattest.Build(fattest.Options{Type: 12}, nil), "fat", "fat12"},
		"builder fat16": {fattest.Build(fattest.Options{Type: 16}, nil), "fat", "fat16"},
		"builder fat32": {fattest.Build(fattest.Options{Type: 32}, nil), "fat", "fat32"},
	}
	for name, f := range map[string]struct{ path, want, typ string }{
		"real ext4 4k":         {"../ext4/testdata/ext4-4k-csum.img.gz", "ext4", "ext4"},
		"real fat12":           {"../fat/testdata/fat12.img.gz", "fat", "fat12"},
		"real fat16":           {"../fat/testdata/fat16.img.gz", "fat", "fat16"},
		"real fat32":           {"../fat/testdata/fat32.img.gz", "fat", "fat32"},
		"real exfat":           {"../exfat/testdata/exfat.img.gz", "exfat", "exfat"},
		"real f2fs":            {"../f2fs/testdata/f2fs-default.img.gz", "f2fs", "f2fs"},
		"real f2fs extra attr": {"../f2fs/testdata/f2fs-extra-attr.img.gz", "f2fs", "f2fs"},
	} {
		images[name] = image{gunzipFixture(t, f.path), f.want, f.typ}
	}

	for name, tc := range images {
		t.Run(name, func(t *testing.T) {
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
