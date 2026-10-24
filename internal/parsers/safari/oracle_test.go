package safari_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
)

func TestSafariMatchesOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.BundleIOS(h, remoteSafari, logicalSafari)
			p := safari.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}

// An application tombstone is the application's own deletion marker (4F-R13): deleted, never live, no
// visit time, and its range only in the deleted_range Times.
func TestTombstoneIsDeletedNeverLiveAndHasNoVisitTime(t *testing.T) {
	for _, v := range []string{"current", "current-wal"} {
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := fx.BundleIOS(h, remoteSafari, logicalSafari)
		p := safari.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		n := 0
		for _, r := range res.Records {
			if !isTombstone(r.Locator) {
				continue
			}
			n++
			if !r.Deleted || r.Recovery != "" || r.Confidence != nil || r.Time != nil || r.TimeEnd != nil {
				t.Errorf("%s %s: deleted %v recovery %q confidence %v time %v", v, r.Locator, r.Deleted, r.Recovery, r.Confidence, r.Time)
			}
			for _, nt := range r.Times {
				if nt.Kind != "deleted_range_start" && nt.Kind != "deleted_range_end" {
					t.Errorf("%s %s: Times kind %q", v, r.Locator, nt.Kind)
				}
			}
			dv, err := webDecode(r)
			if err != nil || dv.Live() {
				t.Errorf("%s %s: decoded live %v err %v", v, r.Locator, dv.Live(), err)
			}
		}
		if n == 0 {
			t.Errorf("%s: no tombstone records", v)
		}
	}
}
