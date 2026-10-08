package parsertest

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// The harness Lookuper applies the host's eligibility: an artifact extracted from a filesystem
// snapshot is available only to a job whose own primary comes from the same snapshot.
func TestHarnessLookuperKeepsSnapshotsApart(t *testing.T) {
	h, live, _ := bundle(t, t, "lk", "one\n")
	p := WellBehaved{Name: "lk", Version: "1.0.0"}
	other := live
	other.ID = "zz-snapshot-copy"
	other.Logical = "android:/data/parsertest/lk-old.dat"
	other.Source.Snapshot = &parse.SnapshotInfo{Name: "snap1", Xid: 5}

	ids := func(in *parse.Input) []string {
		var out []string
		for _, a := range in.Lookup.Find("android:**") {
			out = append(out, a.ID)
		}
		return out
	}

	in := h.Input(p, live, map[string]parse.Artifact{"old": other}, false)
	if got := ids(in); len(got) != 1 || got[0] != live.ID {
		t.Errorf("a live job sees %v, want only %s", got, live.ID)
	}
	found := in.Lookup.Find("android:**")
	if len(found) == 1 {
		if _, err := in.Lookup.Open(other); err == nil {
			t.Error("a live job opened a snapshot artifact")
		}
	}

	snapPrimary := live
	snapPrimary.Source.Snapshot = &parse.SnapshotInfo{Name: "snap1", Xid: 5}
	in = h.Input(p, snapPrimary, map[string]parse.Artifact{"old": other}, false)
	if got := ids(in); len(got) != 2 {
		t.Errorf("a job of the same snapshot sees %v, want both", got)
	}
	otherSnap := other
	otherSnap.ID = "zz-other-snapshot"
	otherSnap.Source.Snapshot = &parse.SnapshotInfo{Name: "snap2", Xid: 9}
	in = h.Input(p, snapPrimary, map[string]parse.Artifact{"old": otherSnap}, false)
	if got := ids(in); len(got) != 1 {
		t.Errorf("a job of snapshot 1 sees %v, want only its own artifact", got)
	}
}
