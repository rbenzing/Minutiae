package apfs_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// inoOf parses the inode number out of an "n:<slot>:<view>:<inode>" entry ID.
func inoOf(t *testing.T, id string) uint64 {
	t.Helper()
	parts := strings.Split(id, ":")
	if len(parts) != 4 {
		t.Fatalf("entry ID %q is not n:<slot>:<view>:<inode>", id)
	}
	n, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ExtentRuns maps the extent records of an object id exactly as Open maps a
// file's: for a file whose private id is its inode number the two are equal.
func TestExtentRunsByIDEqualsOpenRuns(t *testing.T) {
	data := pattern(5*bs+100, 3)
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/frag", Data: data, Fragments: 3})))
	e := mustLookup(t, f, "/Data/frag")
	fl, err := f.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := f.ExtentRuns(0, inoOf(t, e.ID), int64(len(data)))
	if err != nil {
		t.Fatalf("ExtentRuns: %v", err)
	}
	if got, want := runs, fl.Runs(); len(got) != len(want) {
		t.Fatalf("ExtentRuns = %v, Open runs %v", got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("run %d: %v, want %v", i, got[i], want[i])
			}
		}
	}
	if err := filesys.CheckRuns(runs, int64(len(data)), f.Info().Size); err != nil {
		t.Errorf("CheckRuns: %v", err)
	}
}

func TestExtentRunsOfAnObjectWithNoExtentsIsOneHole(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/x", Data: []byte("x")})))
	runs, err := f.ExtentRuns(0, 0x7777, 2*bs) // no such object: a trailing gap is a hole
	if err != nil {
		t.Fatalf("ExtentRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Offset != -1 || runs[0].Length != 2*bs {
		t.Errorf("runs = %v, want one hole of %d", runs, 2*bs)
	}
}
