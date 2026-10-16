package dbrecover

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// unknownCapDB stores a recovered database artifact whose provenance names a
// recovery but no confidence cap, one cause per name (J116, A-M4).
func unknownCapDB(t *testing.T, cause string) (*evidence.Case, Inputs) {
	t.Helper()
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	var rec evidence.ManifestRecord
	switch cause {
	case "no confidence anywhere":
		img := evidencetest.AddImage(t, c, f.db)
		spec := carvedSpec()
		spec.Runs = []evidence.Run{{Offset: 0, Length: int64(len(f.db))}}
		spec.Mutate = func(r *evidence.Recovery) { r.Confidence = nil }
		rec = evidencetest.AddRecovered(t, c, img, f.db, spec)
	case "parent missing":
		// the chain is cut: its parent is not in the manifest, so what lies beyond is unknown
		rec = recordstest.AddDerivedWith(t, c, "x.db", evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
			ParentID: "no-such-parent", ParentSHA256: strings.Repeat("0", 64), Partition: 1, FSType: "mtfs", FSPath: "/x.db", FSID: "dentry:1:1:1",
			Runs:     []evidence.Run{{Offset: 0, Length: int64(len(f.db))}},
			Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Algorithm: evidence.AlgorithmRecover},
		}}, f.db)
	default:
		t.Fatalf("cause %q", cause)
	}
	return c, Inputs{DB: rec.ID, NoSiblings: true}
}

// TestUnknownCapNeverKeepsTheUncappedConfidence (J116): a recovered trail whose
// cap is unknown caps every row at the lowest recovery confidence of the spec
// 3.3 method table of the 3J records (sqlite-page-slack, 25), flags each row
// "recovery-cap-unknown", counts them and says so in the audit.
func TestUnknownCapNeverKeepsTheUncappedConfidence(t *testing.T) {
	for _, cause := range []string{"no confidence anywhere", "parent missing"} {
		t.Run(cause, func(t *testing.T) {
			c, in := unknownCapDB(t, cause)
			res, err := Run(t.Context(), c, in, Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Counts.Written == 0 {
				t.Fatal("no rows written")
			}
			flagged := int64(0)
			for _, f := range storedRecords(t, c) {
				if f.Confidence == nil || *f.Confidence > 25 {
					t.Errorf("record %d confidence %v: an unknown cap must not keep the method confidence", f.ID, f.Confidence != nil && *f.Confidence <= 25)
				}
				if notes, _ := payloadOf(t, f)["notes"].([]any); slices.Contains(notes, any("recovery-cap-unknown")) {
					flagged++
				}
			}
			if flagged != res.Counts.Written || res.Counts.RecoveryCapUnknown != res.Counts.Written {
				t.Errorf("flagged %d counted %d written %d", flagged, res.Counts.RecoveryCapUnknown, res.Counts.Written)
			}
			es := readAudit(t, c)
			if v := lastEntry(t, es, "analysis.end").Details["recovery_cap_unknown"]; fmt.Sprint(v) != fmt.Sprint(res.Counts.Written) {
				t.Errorf("analysis.end recovery_cap_unknown %v, want %d", v, res.Counts.Written)
			}
			warned := false
			for _, i := range indexes(es, "analysis.warning") {
				if r, _ := es[i].Details["reason"].(string); strings.Contains(r, "confidence cap is unknown") {
					warned = true
				}
			}
			if !warned {
				t.Error("no analysis.warning says the cap is unknown")
			}
		})
	}
}

// TestKnownCapIsNotFlaggedUnknown: a trail with a cap is capped by it and carries no flag.
func TestKnownCapIsNotFlaggedUnknown(t *testing.T) {
	c, in, _ := cappedEnv(t)
	res := mustRun(t, c, in, Options{})
	if res.Counts.RecoveryCapUnknown != 0 {
		t.Errorf("RecoveryCapUnknown %d with a known cap", res.Counts.RecoveryCapUnknown)
	}
	for _, f := range storedRecords(t, c) {
		if notes, _ := payloadOf(t, f)["notes"].([]any); slices.Contains(notes, any("recovery-cap-unknown")) {
			t.Errorf("record %d flagged with a known cap", f.ID)
		}
	}
}

// TestTrailCapUnknownCauses (J116): the three causes of a recovered trail with
// no cap, on a hand-built manifest (the case itself refuses a recovered kind
// with no description, so that cause can only be met here).
func TestTrailCapUnknownCauses(t *testing.T) {
	conf := 40
	rv := func(c *int) *evidence.Derivation {
		return &evidence.Derivation{ParentID: "parent", Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Confidence: c}}
	}
	rec := func(id, kind string, d *evidence.Derivation) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: id, Source: evidence.Source{Kind: kind, Derived: d}}
	}
	for name, tc := range map[string]struct {
		man                map[string]evidence.ManifestRecord
		recovered, unknown bool
		cap                int
	}{
		"live":           {map[string]evidence.ManifestRecord{"a": rec("a", "file", nil)}, false, false, 0},
		"no description": {map[string]evidence.ManifestRecord{"a": rec("a", "carve", nil)}, true, true, 0},
		"known":          {map[string]evidence.ManifestRecord{"a": rec("a", "recover", rv(&conf)), "parent": rec("parent", "file", nil)}, true, false, 40},
		"no confidence":  {map[string]evidence.ManifestRecord{"a": rec("a", "recover", rv(nil)), "parent": rec("parent", "file", nil)}, true, true, 0},
		"parent missing": {map[string]evidence.ManifestRecord{"a": rec("a", "recover", rv(nil))}, true, true, 0},
		"cycle":          {map[string]evidence.ManifestRecord{"a": rec("a", "recover", &evidence.Derivation{ParentID: "a", Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile}})}, true, true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			recovered, cp, unknown := trailCap(tc.man, "a")
			if recovered != tc.recovered || unknown != tc.unknown || (cp != nil) != (tc.cap != 0) || (cp != nil && *cp != tc.cap) {
				t.Errorf("recovered %v cap %v unknown %v, want %v %d %v", recovered, cp, unknown, tc.recovered, tc.cap, tc.unknown)
			}
		})
	}
}
