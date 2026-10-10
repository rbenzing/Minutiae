package apfs_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

func longTarget(n int) string { return strings.Repeat("seg/", n/4) + "end" }

func TestSymlinkStreamTarget(t *testing.T) {
	target := longTarget(5000) // over the inline limit: a real driver stores this as a stream
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/long", Symlink: target, SymlinkStream: true},
		apfstest.File{Path: "/short", Symlink: "docs/a.txt"},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	e := byName(t, es, "long")
	if e.Type != filesys.TypeSymlink || e.LinkTarget != target || e.Size != int64(len(target)) {
		t.Fatalf("entry = %+v (target %d bytes)", e.Attrs, len(e.LinkTarget))
	}
	if _, ok := attr(e, "symlink"); ok {
		t.Errorf("a readable stream symlink must not be flagged: %v", e.Attrs)
	}
	fl := openPath(t, f, "/Data/long")
	if got := readAllAt(t, fl); !bytes.Equal(got, []byte(target)) {
		t.Errorf("Open target differs (%d bytes, want %d)", len(got), len(target))
	}
	if len(fl.Runs()) == 0 {
		t.Error("a stream-stored target reads from image blocks and must report their runs")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Errorf("runs: %v", err)
	}
	short := openPath(t, f, "/Data/short")
	if len(short.Runs()) != 0 {
		t.Errorf("an embedded target has no runs: %v", short.Runs())
	}
	noWarnings(t, f)
}

func TestSymlinkStreamHostile(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/nodata", Symlink: "x", SymlinkStream: true, SymlinkStreamNoData: true},
		apfstest.File{Path: "/huge", Symlink: longTarget(1 << 17), SymlinkStream: true},
		apfstest.File{Path: "/notarget", Mode: 0o120777},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	for _, n := range []string{"nodata", "huge"} {
		e := byName(t, es, n)
		if e.LinkTarget != "" {
			t.Errorf("%s: LinkTarget = %q, want none", n, e.LinkTarget)
		}
		if v, ok := attr(e, "symlink"); !ok || v != "unreadable" {
			t.Errorf("%s: attrs %v, want symlink=unreadable", n, e.Attrs)
		}
	}
	if _, err := f.Open(mustLookup(t, f, "/Data/nodata")); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a stream with no extents: %v, want ErrCorrupt", err)
	}
	if _, err := f.Open(mustLookup(t, f, "/Data/huge")); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("a target over the bound: %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(mustLookup(t, f, "/Data/notarget")); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a symlink without a target: %v, want ErrCorrupt", err)
	}
	if !hasWarn(f, "symlink") {
		t.Errorf("no warning naming the unreadable stream targets: %q", f.Info().Warnings)
	}
}

func TestSymlinkStreamBudgetIsCumulativeAndReported(t *testing.T) {
	target := longTarget(5000)
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/a", Symlink: target, SymlinkStream: true},
		apfstest.File{Path: "/b", Symlink: target, SymlinkStream: true},
	)))
	f.SetLinkBudget(int64(len(target) + 1)) // room for exactly one target (with its NUL)
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	var read, refused int
	for _, n := range []string{"a", "b"} {
		e := byName(t, es, n)
		if e.LinkTarget == target {
			read++
		}
		if v, _ := attr(e, "symlink"); v == "unreadable" {
			refused++
		}
	}
	if read != 1 || refused != 1 {
		t.Errorf("%d targets read and %d refused with a budget for one", read, refused)
	}
	if !hasWarn(f, "budget") {
		t.Errorf("a spent target budget must be warned about: %q", f.Info().Warnings)
	}
	fl, err := f.Open(mustLookup(t, f, "/Data/b"))
	if !errors.Is(err, filesys.ErrUnsupported) || fl != nil {
		t.Errorf("Open of a target past the budget = %v, %v; want ErrUnsupported and no file", fl, err)
	}
}
