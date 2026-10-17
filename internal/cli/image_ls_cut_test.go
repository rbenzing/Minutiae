package cli

import (
	"context"
	"encoding/json"
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

// failRootFS cannot list the root at all: no entries, only an error.
type failRootFS struct{ filesys.FileSystem }

func (c failRootFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	if d.ID == c.FileSystem.Root().ID {
		return nil, fmt.Errorf("directory unreadable: %w", filesys.ErrLimit)
	}
	return c.FileSystem.ReadDir(d)
}

func (c failRootFS) Underlying() filesys.FileSystem { return c.FileSystem }

func driversWrapping(wrap func(filesys.FileSystem) filesys.FileSystem) []detect.Driver {
	return []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		fsys, err := fstest.Open(r, size)
		if err != nil {
			return nil, err
		}
		return wrap(fsys), nil
	}}}
}

// F87: "listing incomplete" claims a partial listing; a total failure printed no entries, so it prints
// the error alone.
func TestImageLsTotalFailureDoesNotClaimAPartialListing(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return failRootFS{f} })
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "ls", "--case", e.c, e.ref)
	if strings.Contains(errs, "listing incomplete") || strings.TrimSpace(out) != "" {
		t.Errorf("a total failure claims a partial listing:\nstdout %q\nstderr %q", out, errs)
	}
	if !strings.Contains(errs, "directory unreadable") || code != ExitError {
		t.Errorf("exit %d, stderr %q; want the error and exit %d", code, errs, ExitError)
	}
}

// F88: a recursive ls of a cut directory prints everything it read, warns, and exits with the error's code,
// as the non-recursive form does.
func TestImageLsRecursiveCutListingExitsWithTheErrorCode(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return cutRootFS{f} })
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "ls", "-r", "--case", e.c, e.ref)
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

// cutDocsFS cuts the listing of /docs (it returns the entries it has together with an error).
type cutDocsFS struct{ filesys.FileSystem }

func (c cutDocsFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	kids, err := c.FileSystem.ReadDir(d)
	if err == nil && d.Name == "docs" {
		err = fmt.Errorf("directory entry cap reached: %w", filesys.ErrLimit)
	}
	return kids, err
}

func (c cutDocsFS) Underlying() filesys.FileSystem { return c.FileSystem }

// F89: image stat of a deleted entry by id, after an incomplete search, shows the entry and says a live entry
// with this id may exist in an unread directory (text and JSON), and exits 0 (partial work is kept).
func TestImageStatDeletedByIDAfterCutShowsTheIncompleteness(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return cutDocsFS{f} })
	_, lsOut := run(t, e.d, "image", "ls", "--case", e.c, "--deleted", "--json", e.ref)
	var ents []struct {
		Path  string
		Entry struct{ ID string }
	}
	if err := json.Unmarshal(jsonPart(lsOut), &ents); err != nil {
		t.Fatalf("ls json %q: %v", lsOut, err)
	}
	id := ""
	for _, en := range ents {
		if en.Path == "/gone.txt" {
			id = en.Entry.ID
		}
	}
	if id == "" {
		t.Fatalf("no /gone.txt in %q", lsOut)
	}
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "stat", "--case", e.c, e.ref, "id:"+id)
	if code != 0 || !strings.Contains(out, "/gone.txt") {
		t.Fatalf("stat: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if !strings.Contains(errs+out, "a live entry with this id may exist") || !strings.Contains(errs+out, "/docs") {
		t.Errorf("no incompleteness shown:\nstdout %q\nstderr %q", out, errs)
	}
	code, out, _ = runSplit(context.Background(), t, e.d, "image", "stat", "--case", e.c, "--json", e.ref, "id:"+id)
	if code != 0 || !strings.Contains(out, "a live entry with this id may exist") {
		t.Errorf("json stat: exit %d, %q", code, out)
	}
}

// F89: image recover --list and the run, for an id whose search was cut, keep the deleted match, say that a live
// entry with this id may exist in an unread directory, and exit 0; the JSON carries it.
func TestImageRecoverByIDAfterCutShowsTheIncompleteness(t *testing.T) {
	e := newRecoverEnv(t, recNode("/g.bin", make([]byte, 700)), fstest.Node{Path: "/docs/a.txt", Data: []byte("a")})
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return cutDocsFS{f} })
	_, lsOut := run(t, e.d, "image", "ls", "--case", e.c, "--deleted", "--json", e.ref)
	var ents []struct {
		Path  string
		Entry struct{ ID string }
	}
	if err := json.Unmarshal(jsonPart(lsOut), &ents); err != nil {
		t.Fatalf("ls json %q: %v", lsOut, err)
	}
	id := ""
	for _, en := range ents {
		if en.Path == "/g.bin" {
			id = en.Entry.ID
		}
	}
	if id == "" {
		t.Fatalf("no /g.bin in %q", lsOut)
	}
	for _, mode := range []string{"--list", "--run"} {
		args := []string{"image", "recover", "--case", e.c, e.ref, "id:" + id}
		if mode == "--list" {
			args = append(args, mode)
		}
		code, out, errs := runSplit(context.Background(), t, e.d, args...)
		if code != 0 || !strings.Contains(out+errs, "a live entry with this id may exist") || !strings.Contains(out+errs, "/docs") {
			t.Errorf("%s: exit %d\nstdout %q\nstderr %q", mode, code, out, errs)
		}
		code, out, _ = runSplit(context.Background(), t, e.d, append(args, "--json")...)
		if code != 0 || !strings.Contains(out, `"incomplete_ids"`) || !strings.Contains(out, "a live entry with this id may exist") {
			t.Errorf("%s --json: exit %d, %q", mode, code, out)
		}
	}
}
