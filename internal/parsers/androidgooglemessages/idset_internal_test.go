package androidgooglemessages

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// B6, R12 (Task 6 review I1): every cause that leaves the decided message-id set incomplete makes the parts pass state a part
// not yet joined as undecided with the cause, never as an orphan record: an unreadable messages rowid, a messages row that
// was not mapped (a join error that is not a damage, the one error a messages mapping returns on a readable row) and a
// refused budget charge. Constructible at Parse level are the unreadable table (TestGoogleMessagesUnreadableMessagesTableIsNoOrphan)
// only: a rowid alias is never unreadable in a real database and the host's own budget is spent by the records before it
// is by the ids.
func TestGoogleMessagesIncompleteIDSetCausesAreStated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
		run   func(e *env, in *parse.Input)
	}{
		{"unreadable rowid", "messages _id unreadable", func(e *env, in *parse.Input) {
			mp := messagesMapping(e)
			recs, err := mp.Map(mapCtx{in: in, table: mp.Table, cols: mp.Columns}, fakeRow{table: mp.Table, cells: goodCells(mp), noRowid: true})
			if err != nil || len(recs) != 1 {
				t.Fatalf("the messages row: %v %d records", err, len(recs))
			}
		}},
		{"row not mapped", "a messages row was not mapped", func(e *env, in *parse.Input) {
			mp := messagesMapping(e)
			ctx := mapCtx{in: in, table: mp.Table, cols: mp.Columns, failTable: tableParts, failMatch: errors.New("boom")}
			if _, err := mp.Map(ctx, fakeRow{table: mp.Table, cells: goodCells(mp)}); err == nil {
				t.Fatal("the messages row mapped although its parts join failed")
			}
		}},
		{"budget", "budget", func(e *env, in *parse.Input) {
			mp := messagesMapping(e)
			if _, err := mp.Map(mapCtx{in: in, table: mp.Table, cols: mp.Columns}, fakeRow{table: mp.Table, cells: goodCells(mp)}); err != nil {
				t.Fatalf("the messages row: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fakeInput()
			if tc.name == "budget" {
				in.Budget = parse.NewBudget(0).View() // not even one id is admitted
			}
			e := fakeEnv(in)
			tc.run(e, in)
			if e.IDs.complete || e.IDs.cause != tc.cause {
				t.Fatalf("complete %v cause %q, want incomplete by %q", e.IDs.complete, e.IDs.cause, tc.cause)
			}
			pm := partsMapping(e)
			recs, err := pm.Map(mapCtx{in: in, table: pm.Table, cols: pm.Columns}, fakeRow{table: pm.Table, cells: goodCells(pm)})
			var re *rowrun.RowError
			if !errors.As(err, &re) || len(recs) != 0 || !strings.Contains(re.Reason, "message-id set is incomplete ("+tc.cause+")") {
				t.Errorf("the part of message 1: %d records, err %v, want a row warning naming %q (never an orphan record)", len(recs), err, tc.cause)
			}
			if e.Counts.Orphans != 0 {
				t.Errorf("%d orphans under an incomplete set", e.Counts.Orphans)
			}
		})
	}
}

// Task 6 review M3: a cut that falls on a character boundary drops nothing; one inside a character drops that character.
func TestTrimPartialRuneKeepsACompleteLastCharacter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"ab\u00e9", "ab\u00e9"},
		{"ab\u20ac", "ab\u20ac"},
		{"ab\U0001F600", "ab\U0001F600"},
		{"ab\xc3", "ab"},
		{"a\xe2\x82", "a"},
		{"a\xf0\x9f\x98", "a"},
		{"a\xf0\x9f", "a"},
		{"\x80\x80\x80\x80\x80", "\x80\x80\x80\x80\x80"}, // stray continuation bytes are not a cut character
	} {
		if got := string(trimPartialRune([]byte(tc.in))); got != tc.want {
			t.Errorf("trimPartialRune(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// joinText: a first part of exactly the keep limit, cut on a boundary, loses no character
	first := []byte(strings.Repeat("\u00e9", maxBody)) // 2*maxBody bytes
	joined, total := joinText([][]byte{first, []byte("x")})
	if len(joined) != 2*maxBody || total != int64(2*maxBody+2) {
		t.Errorf("joinText kept %d bytes of %d, want %d of %d", len(joined), total, 2*maxBody, 2*maxBody+2)
	}
	// a cut inside a character keeps whole characters only
	joined, _ = joinText([][]byte{[]byte(strings.Repeat("\u00e9", maxBody-1)), []byte("\u20ac\u20ac")})
	if got := string(joined); strings.ContainsRune(got, '\uFFFD') || len(joined) > 2*maxBody {
		t.Errorf("joinText kept %d bytes, a broken character in them: %v", len(joined), strings.ContainsRune(got, '\uFFFD'))
	}
}
