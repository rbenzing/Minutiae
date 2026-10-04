//go:build realimages

package hfsplus_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
)

// TestRealHFSPlusImages reads every *.hfs, *.hfsplus and *.img file under
// $MINUTIAE_TEST_IMAGES/hfsplus (run it by hand on macOS-made volumes and old
// iOS user-partition images): it opens each one, walks it, reads every file,
// checks File.Runs and Unallocated, fails on a panic or a contract violation
// and logs the warnings the reader raised. Run:
//
//	MINUTIAE_TEST_IMAGES=/path go test -tags realimages -run TestRealHFSPlusImages ./internal/filesys/hfsplus/
func TestRealHFSPlusImages(t *testing.T) {
	root := os.Getenv("MINUTIAE_TEST_IMAGES")
	if root == "" {
		t.Skip("MINUTIAE_TEST_IMAGES is not set")
	}
	var paths []string
	for _, pat := range []string{"*.hfs", "*.hfsplus", "*.img"} {
		m, err := filepath.Glob(filepath.Join(root, "hfsplus", pat))
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, m...)
	}
	if len(paths) == 0 {
		t.Skipf("no images under %s", filepath.Join(root, "hfsplus"))
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			fsys, err := hfsplus.Open(f, st.Size())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			info := fsys.Info()
			t.Logf("%s %q %d-byte blocks, %d bytes, features %v", info.Type, info.Label, info.BlockSize, info.Size, info.Features)
			var dirs, files, symlinks, unreadable int
			var bytesRead int64
			err = filesys.Walk(fsys, fsys.Root(), "/", func(path string, e filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("walk %s: %v", path, err)
					return nil
				}
				switch e.Type {
				case filesys.TypeDir:
					dirs++
					return nil
				case filesys.TypeSymlink:
					symlinks++
				case filesys.TypeFile:
					files++
				default:
					return nil
				}
				file, err := fsys.Open(e)
				if err != nil {
					if !errors.Is(err, filesys.ErrUnsupported) && !errors.Is(err, filesys.ErrCorrupt) {
						t.Errorf("open %s: %v", path, err)
					}
					unreadable++
					return nil
				}
				checkFuzzRuns(t, path, file, info.Size)
				buf := make([]byte, min(file.Size(), 1<<20))
				for off := int64(0); off < file.Size(); off += int64(len(buf)) {
					n, rerr := file.ReadAt(buf, off)
					bytesRead += int64(n)
					if rerr != nil && !errors.Is(rerr, filesys.ErrCorrupt) && !errors.Is(rerr, io.EOF) {
						t.Errorf("read %s at %d: %v", path, off, rerr)
					}
					if rerr != nil {
						break
					}
				}
				return nil
			})
			if err != nil {
				t.Errorf("walk: %v", err)
			}
			runs, err := fsys.Unallocated()
			if err != nil && !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("Unallocated: %v", err)
			}
			var free int64
			for _, r := range runs {
				if r.Length <= 0 || r.Offset < 0 || r.Offset+r.Length > info.Size {
					t.Errorf("free run %+v outside the filesystem", r)
				}
				free += r.Length
			}
			t.Logf("%d dirs, %d files, %d symlinks (%d not readable), %d bytes read, %d free bytes in %d runs", dirs, files, symlinks, unreadable, bytesRead, free, len(runs))
			for _, w := range fsys.Info().Warnings {
				t.Logf("warning: %s", w)
			}
		})
	}
}
