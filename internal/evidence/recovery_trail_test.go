package evidence_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestRecoveryTrailCut: Cut says the walk ended on a missing parent, a cycle or
// the depth bound, never on an artifact with no derivation (3J residual R1).
func TestRecoveryTrailCut(t *testing.T) {
	rec := func(c int) *evidence.Recovery {
		return &evidence.Recovery{Class: evidence.ClassCarved, Method: "carve-x", Confidence: &c}
	}
	m := func(id, parent string, r *evidence.Recovery) evidence.ManifestRecord {
		var d *evidence.Derivation
		if parent != "" {
			d = &evidence.Derivation{ParentID: parent, Recovery: r}
		}
		return evidence.ManifestRecord{ID: id, Source: evidence.Source{Kind: "x", Derived: d}}
	}
	set := func(rs ...evidence.ManifestRecord) map[string]evidence.ManifestRecord {
		out := map[string]evidence.ManifestRecord{}
		for _, r := range rs {
			out[r.ID] = r
		}
		return out
	}
	// a0 <- a1 <- ... ; the carrier (confidence 44) is carrierAt hops above a0, then root, or a missing parent
	chain := func(carrierAt int, rooted bool) map[string]evidence.ManifestRecord {
		var rs []evidence.ManifestRecord
		for i := 0; i <= carrierAt; i++ {
			var r *evidence.Recovery
			if i == carrierAt {
				r = rec(44)
			}
			rs = append(rs, m(fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", i+1), r))
		}
		if rooted {
			rs = append(rs, m(fmt.Sprintf("a%d", carrierAt+1), "", nil))
		}
		return set(rs...)
	}
	for _, tc := range []struct {
		name string
		man  map[string]evidence.ManifestRecord
		id   string
		cut  bool
		min  int
	}{
		{"clean chain to the image", set(m("img", "", nil), m("a", "img", rec(50))), "a", false, 50},
		{"missing parent after a carrier", set(m("a", "gone", rec(80))), "a", true, 80},
		{"cycle after a carrier", set(m("a", "b", rec(30)), m("b", "a", nil)), "b", true, 30},
		{"root reached at the sixteenth hop", chain(15, true), "a0", false, 44},
		{"root beyond the sixteenth hop", chain(16, true), "a0", true, 44},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, ok := evidence.RecoveryTrailOf(tc.man, tc.id)
			if !ok || tr.Cut != tc.cut || tr.MinConfidence == nil || *tr.MinConfidence != tc.min {
				t.Fatalf("%+v %v, want cut %v min %d", tr, ok, tc.cut, tc.min)
			}
		})
	}
	t.Run("depth bound after a carrier", func(t *testing.T) {
		// carrier at hop 3, the chain continues past the bound with no end
		var rs []evidence.ManifestRecord
		for i := 0; i < 40; i++ {
			var r *evidence.Recovery
			if i == 3 {
				r = rec(44)
			}
			rs = append(rs, m(fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", i+1), r))
		}
		tr, ok := evidence.RecoveryTrailOf(set(rs...), "a0")
		if !ok || !tr.Cut || tr.MinConfidence == nil || *tr.MinConfidence != 44 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
}
