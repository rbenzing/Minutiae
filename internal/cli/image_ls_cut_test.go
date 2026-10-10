package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// cutRootFS lists the root with its entries and a cut error, as a directory
// over the entry cap does.
type cutRootFS struct{ filesys.FileSystem }

func (c cutRootFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	kids, err := c.FileSystem.ReadDir(d)
	if err == nil && d.ID == c.FileSystem.Root().ID {
		err = fmt.Errorf("directory entry cap reached: %w", filesys.ErrLimit)
	}
	return kids, err
}

func (c cutRootFS) Underlying() filesys.FileSystem { return c.FileSystem }

// F83: a non-recursive ls of a cut listing prints the entries read, then a
// warning naming the cut reason, and exits with the error's code.
func TestImageLsCutListingPrintsPrefixThenFailsWithWarning(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		fsys, err := fstest.Open(r, size)
		if err != nil {
			return nil, err
		}
		return cutRootFS{fsys}, nil
	}}}
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "ls", "--case", e.c, e.ref)
	if !strings.Contains(out, "top.txt") || !strings.Contains(out, "docs") {
		t.Errorf("stdout lacks the entries read:\n%s", out)
	}
	if !strings.Contains(errs, "warning:") || !strings.Contains(errs, "structural limit exceeded") {
		t.Errorf("stderr lacks the warning naming the cut reason:\n%s", errs)
	}
	if code != ExitError {
		t.Errorf("exit %d, want %d", code, ExitError)
	}
}
