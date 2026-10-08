package sqlitefile_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// End to end, a live lookup that hits a structural limit (a payload over
// Limits.MaxPayloadBytes) cannot answer: the history row is unknown with
// live-lookup-uncertain, the pass finishes, and it is never absent-from-live
// (ruling C49). The lookup reports the limit as an answerless corruption; the
// sentinels ErrLimit and ErrPageUnavailable themselves cannot reach the
// comparison through a lookup (see isAnswerless and its unit test).
func TestLiveLookupPayloadOverTheCapIsAnswerless(t *testing.T) {
	s := newRowScen(t)
	s.t.Update(2, int64(20), strings.Repeat("x", 300)) // the live row 2 declares a payload over the cap
	s1 := s.b.Snapshot()
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), s1.Pages())
	db := walMode(s.db0)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxPayloadBytes: 200}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	tb, err := d.Live().Table(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tb.Get(t.Context(), 2); !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Fatalf("live Get(2): %v, want an answerless corruption", err)
	}
	rows, st := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if r.Table != "t" || r.Rowid == nil || *r.Rowid != 2 {
			continue
		}
		n++
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteLiveUncertain) {
			t.Errorf("relation %s notes %v, want unknown with %s", r.Relation, r.Notes, sqlitefile.NoteLiveUncertain)
		}
	}
	if n == 0 || st.Unknown == 0 {
		t.Fatalf("%d history rows of rowid 2, stats %+v", n, st)
	}
}
