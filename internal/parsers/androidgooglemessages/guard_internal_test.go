package androidgooglemessages

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// stubCtx answers every join with a clean miss, to show which ones the guard keeps from reaching it.
type stubCtx struct{ parse.MapContext }

func (stubCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (stubCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

// Spec 6.4 to 6.6: a join through a guarded table is undecided, never a clean miss; an unguarded table is untouched.
func TestGuardedContextMakesGuardedJoinsUndecided(t *testing.T) {
	g := &joinGuard{}
	g.add("Participants", "column _id is not the rowid alias")
	c := guardedCtx{MapContext: stubCtx{}, g: g}
	if _, _, err := c.Get("participants", 1); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("Get on a guarded table: err %v", err)
	}
	if _, err := c.Match("PARTICIPANTS", "_id", parse.JoinKey{}); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("Match on a guarded table: err %v", err)
	}
	if _, found, err := c.Get("conversations", 1); err != nil || found {
		t.Errorf("Get on an unguarded table: found %v, err %v", found, err)
	}
	if n := c.UndecidedHits("participants", "_id"); n != 0 {
		t.Errorf("UndecidedHits = %d", n)
	}
	if r, ok := (*joinGuard)(nil).reason("participants"); ok || r != "" {
		t.Errorf("a nil guard guards %q", r)
	}
}

// Spec 5.4: the signature names the five core tables and checks the destination columns as a set; the mapped columns
// include every signature column, so a missing one is never listed as optional.
func TestMappedColumnsContainTheSignature(t *testing.T) {
	for _, rq := range reqs() {
		have := map[string]bool{}
		for _, c := range mappedColumns(rq.Table) {
			have[c] = true
		}
		for _, c := range rq.Need {
			if !have[c] {
				t.Errorf("%s.%s is required by the signature and not in mappedColumns", rq.Table, c)
			}
		}
	}
	for _, c := range destinationColumns {
		found := false
		for _, m := range mappedColumns(tableParticipants) {
			found = found || m == c
		}
		if !found {
			t.Errorf("participants.%s is not mapped", c)
		}
	}
}
