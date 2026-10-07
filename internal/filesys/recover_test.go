package filesys_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

func cand(size int64, runs ...filesys.Run) filesys.Candidate {
	return filesys.Candidate{Method: "fat-contiguous", Size: size, Runs: runs}
}

func TestCheckCandidateTable(t *testing.T) {
	const fsSize = 10000
	valid := []struct {
		name    string
		c       filesys.Candidate
		covered int64
	}{
		{"contiguous", cand(100, filesys.Run{Offset: 512, Length: 100}), 100},
		{"fragmented", cand(300, filesys.Run{Offset: 4096, Length: 100}, filesys.Run{Offset: 512, Length: 200}), 300},
		{"exact prefix", cand(300, filesys.Run{Offset: 512, Length: 100}), 100},
		{"size 0 no runs", cand(0), 0},
		{"size equals fs", cand(fsSize, filesys.Run{Offset: 0, Length: fsSize}), fsSize},
		{"run ends exactly at fs end", cand(10, filesys.Run{Offset: fsSize - 10, Length: 10}), 10},
		{"method first char a", candMethod("ab"), 0},
		{"method first char z", candMethod("zz"), 0},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			got, err := filesys.CheckCandidate(tc.c, fsSize)
			if err != nil || got != tc.covered {
				t.Fatalf("covered %d err %v, want %d nil", got, err, tc.covered)
			}
		})
	}
	many := make([]filesys.Run, filesys.MaxCandidateRuns+1)
	for i := range many {
		many[i] = filesys.Run{Offset: int64(i), Length: 1}
	}
	withMethod := func(m string) filesys.Candidate {
		c := cand(0)
		c.Method = m
		return c
	}
	withBasis := func(b ...string) filesys.Candidate {
		c := cand(0)
		c.Basis = b
		return c
	}
	withAssumptions := func(a ...string) filesys.Candidate {
		c := cand(0)
		c.Assumptions = a
		return c
	}
	invalid := []struct {
		name, reason string
		c            filesys.Candidate
	}{
		{"negative offset", "run 0 has offset -5", cand(10, filesys.Run{Offset: -5, Length: 10})},
		{"hole", "run 1 is a hole", cand(20, filesys.Run{Offset: 0, Length: 10}, filesys.Run{Offset: -1, Length: 10})},
		{"length 0", "run 0 has length 0", cand(10, filesys.Run{Offset: 0, Length: 0})},
		{"length negative", "run 0 has length -1", cand(10, filesys.Run{Offset: 0, Length: -1})},
		{"overflow", "lies outside", cand(10, filesys.Run{Offset: math.MaxInt64, Length: 2})},
		{"beyond fs", "lies outside", cand(10, filesys.Run{Offset: fsSize - 5, Length: 10})},
		{"run ends one past fs end", "lies outside", cand(10, filesys.Run{Offset: fsSize - 9, Length: 10})},
		{"one-byte overlap", "runs 0 and 1 overlap", cand(20, filesys.Run{Offset: 0, Length: 10}, filesys.Run{Offset: 9, Length: 10})},
		{"one-byte overlap reversed", "overlap", cand(20, filesys.Run{Offset: 9, Length: 10}, filesys.Run{Offset: 0, Length: 10})},
		{"method first char before a", "method", withMethod("`b")},
		{"method first char after z", "method", withMethod("{b")},
		{"sum over size", "cover more than the size", cand(10, filesys.Run{Offset: 0, Length: 6}, filesys.Run{Offset: 100, Length: 6})},
		{"sum one over size", "cover more than the size", cand(10, filesys.Run{Offset: 0, Length: 6}, filesys.Run{Offset: 100, Length: 5})},
		{"size over fs", "exceeds the", cand(fsSize + 1)},
		{"negative size", "negative size", cand(-1)},
		{"too many runs", "too many runs", cand(int64(len(many)), many...)},
		{"overlap in order", "runs 0 and 1 overlap", cand(20, filesys.Run{Offset: 0, Length: 10}, filesys.Run{Offset: 5, Length: 10})},
		{"overlap reversed", "overlap", cand(20, filesys.Run{Offset: 5, Length: 10}, filesys.Run{Offset: 0, Length: 10})},
		{"duplicate run", "runs 0 and 1 overlap", cand(20, filesys.Run{Offset: 0, Length: 10}, filesys.Run{Offset: 0, Length: 10})},
		{"empty method", "method", withMethod("")},
		{"method upper", "method", withMethod("Fat")},
		{"method digit first", "method", withMethod("1ab")},
		{"method one char", "method", withMethod("a")},
		{"method too long", "method", withMethod("a" + strings.Repeat("b", 32))},
		{"method underscore", "method", withMethod("fat_x")},
		{"65 basis", "basis", withBasis(make([]string, 65)...)},
		{"257-byte basis", "basis", withBasis(strings.Repeat("x", 257))},
		{"257-byte assumption", "assumption", withAssumptions(strings.Repeat("x", 257))},
		{"65 assumptions", "assumption", withAssumptions(make([]string, 65)...)},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			_, err := filesys.CheckCandidate(tc.c, fsSize)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err %v, want it to contain %q", err, tc.reason)
			}
			var ce *filesys.CorruptError
			if !errors.As(err, &ce) || ce.Structure != "recovery map" || !errors.Is(err, filesys.ErrCorrupt) {
				t.Fatalf("err %#v is not a recovery-map CorruptError", err)
			}
		})
	}
	// Boundaries that must stay valid.
	ok := withMethod("a" + strings.Repeat("b", 31))
	ok.Basis = make([]string, 64)
	ok.Assumptions = []string{strings.Repeat("x", 256)}
	if _, err := filesys.CheckCandidate(ok, fsSize); err != nil {
		t.Errorf("boundary candidate refused: %v", err)
	}
	if _, err := filesys.CheckCandidate(cand(5, filesys.Run{Offset: 0, Length: 5}), -1); err == nil {
		t.Error("negative filesystem size accepted")
	}
}

func TestCheckCandidateAdjacentRunsAreNotOverlap(t *testing.T) {
	c := cand(20, filesys.Run{Offset: 10, Length: 10}, filesys.Run{Offset: 0, Length: 10})
	if got, err := filesys.CheckCandidate(c, 100); err != nil || got != 20 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestCheckCandidateDoesNotModifyItsInput(t *testing.T) {
	runs := []filesys.Run{{Offset: 50, Length: 10}, {Offset: 0, Length: 10}}
	if _, err := filesys.CheckCandidate(cand(20, runs...), 100); err != nil {
		t.Fatal(err)
	}
	if runs[0].Offset != 50 {
		t.Errorf("runs were reordered: %v", runs)
	}
}

func TestErrDeletedNamesImageRecover(t *testing.T) {
	if got := filesys.ErrDeleted.Error(); got != "entry is deleted (use image recover)" {
		t.Errorf("ErrDeleted = %q", got)
	}
}

func TestErrNoRecoveryIsUnsupported(t *testing.T) {
	if !errors.Is(filesys.ErrNoRecovery, filesys.ErrUnsupported) || !errors.Is(filesys.ErrNoJournal, filesys.ErrUnsupported) {
		t.Error("ErrNoRecovery and ErrNoJournal must wrap ErrUnsupported")
	}
	if !strings.Contains(filesys.ErrNoRecovery.Error(), "does not support recovery") || !strings.Contains(filesys.ErrNoJournal.Error(), "keeps no journal") {
		t.Errorf("texts %q, %q", filesys.ErrNoRecovery, filesys.ErrNoJournal)
	}
}

func TestErrNotDeletedIsNotCorrupt(t *testing.T) {
	e := filesys.ErrNotDeleted
	if e.Error() != "entry is not deleted" || errors.Is(e, filesys.ErrCorrupt) || errors.Is(e, filesys.ErrNotFound) || errors.Is(e, filesys.ErrDeleted) {
		t.Errorf("ErrNotDeleted = %v", e)
	}
}

func candMethod(m string) filesys.Candidate {
	c := cand(0)
	c.Method = m
	return c
}
