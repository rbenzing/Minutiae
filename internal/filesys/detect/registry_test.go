package detect_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// The probe order is the spec's (apfs, f2fs, ext4, exfat, hfsplus, fat) over the
// drivers that exist, and each driver claims only its own images.
func TestDriversOrderAndProbes(t *testing.T) {
	var names []string
	for _, d := range detect.Drivers {
		names = append(names, d.Name)
	}
	if want := []string{"ext4", "exfat", "fat"}; !slices.Equal(names, want) {
		t.Fatalf("driver order = %v, want %v", names, want)
	}

	for name, tc := range map[string]struct {
		img  []byte
		want string // detect driver name
		typ  string // Info().Type
	}{
		"ext4":  {ext4test.Build(ext4test.Options{Extents: true}, nil), "ext4", "ext4"},
		"exfat": {exfattest.Build(exfattest.Options{}, nil), "exfat", "exfat"},
		"fat12": {fattest.Build(fattest.Options{Type: 12}, nil), "fat", "fat12"},
		"fat16": {fattest.Build(fattest.Options{Type: 16}, nil), "fat", "fat16"},
		"fat32": {fattest.Build(fattest.Options{Type: 32}, nil), "fat", "fat32"},
	} {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(tc.img)
			if got, ok := detect.Probe(r, int64(len(tc.img))); !ok || got != tc.want {
				t.Fatalf("Probe = %q, %v; want %q", got, ok, tc.want)
			}
			fsys, err := detect.Open(r, int64(len(tc.img)))
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
