package examine

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestCarveEntryLimitFitsRecoveryText: the longest entry the scanner passes on
// (its limit plus the cut marker) must pass the case's recovery text check, so
// the two limits cannot drift apart (whole-branch finding 1).
func TestCarveEntryLimitFitsRecoveryText(t *testing.T) {
	check := func(n int) []string {
		s := strings.Repeat("x", n)
		r := &evidence.Recovery{Class: evidence.ClassCarved, Method: "carve-validated", Basis: []string{s}, Assumptions: []string{s}}
		var long []string
		for _, p := range r.Check(evidence.KindCarve) {
			if strings.Contains(p, "bytes (at most") {
				long = append(long, p)
			}
		}
		return long
	}
	if p := check(carve.MaxEntryBytes + carve.MaxEntryMarkerBytes); len(p) != 0 {
		t.Errorf("an entry of the carve limit plus marker is refused by the case: %v", p)
	}
	if len(check(1<<12)) == 0 { // the probe sees the length rule at all
		t.Error("the recovery text check no longer refuses a long entry: this test would pass vacuously")
	}
}

// TestCarveHostileGzipNameDoesNotAbortTheScope: a gzip FNAME of 0xFF bytes
// escapes to a long basis; the carve completes the scope, writes the objects
// before and after it, and the case verifies.
func TestCarveHostileGzipNameDoesNotAbortTheScope(t *testing.T) {
	hostile := carvetest.GzipHeader(0x08, strings.Repeat("\xff", 64), "")
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	nodes = append(nodes, carveDeleted("/del3.gz", hostile))
	e := newCarveEnv(t, nodes...)
	cs := e.carve(t, CarveOptions{MinConfidence: 25})
	if cs.Carved < 3 {
		t.Fatalf("summary %+v, want the JPEG, the PNG and the gzip carved", cs)
	}
	var gz int
	for _, r := range carvedRecords(t, e.c) {
		if strings.HasSuffix(r.Path, ".gz") {
			gz++
			for _, b := range r.Source.Derived.Recovery.Basis {
				if len(b) > carve.MaxEntryBytes+carve.MaxEntryMarkerBytes {
					t.Errorf("basis entry of %d bytes", len(b))
				}
			}
		}
	}
	if gz != 1 {
		t.Errorf("%d gzip artifacts, want 1", gz)
	}
	if rep := carveVerify(t, e); !rep.OK() {
		t.Errorf("verify: %v", rep.Problems)
	}
}
