package androidtelegram

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// keyCtx is a MapContext whose Match on <table>.mid answers rows (each a fakeRow of [mid, uid] cells), err, and, when
// lossy, a hit on an index that lost rows (UndecidedHits grows by one per Match, as sqlitedb.Context reports it). It
// keeps the notes it is given.
type keyCtx struct {
	rows  []fakeRow
	err   error
	lossy bool
	hits  int64
	notes map[string]string
}

func (c *keyCtx) Input() *parse.Input { return nil }
func (c *keyCtx) Note(k, v string) {
	if _, dup := c.notes[k]; !dup {
		c.notes[k] = v
	}
}

func (c *keyCtx) Column(_, col string) int {
	switch col {
	case "mid":
		return 0
	case "uid":
		return 1
	}
	return -1
}

func (c *keyCtx) Get(string, int64) (parse.Row, bool, error) { return nil, false, nil }

func (c *keyCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.lossy {
		c.hits++
	}
	out := make([]parse.Row, len(c.rows))
	for i, r := range c.rows {
		out[i] = r
	}
	return out, nil
}

func (c *keyCtx) UndecidedHits(string, string) int64 { return c.hits }

func keyRow(table string, mid, uid int64) fakeRow {
	return fakeRow{table: table, cells: []cell{{kind: cInt, i: mid}, {kind: cInt, i: uid}}}
}

// Review I1, M1, M2 (Task 14 fix round 1; rulings T2, T9): liveKeyState is present only on an equal key (sound on a
// short answer too), undecided on a failed lookup or a short answer without the key, absent only on a decided miss.
// messages_v2 is keyed (mid, uid); messages (pre-84, REPLACE INTO on the mid rowid) by mid alone. A failed lookup is
// stated once per run in the note live_key_undecided.<table>.mid with its cause.
func TestLiveKeyStateDecidesOnlyOnACompleteAnswer(t *testing.T) {
	const mid, uid = 101, 1000002
	for _, c := range []struct {
		name  string
		table string
		ctx   keyCtx
		ok    bool
		want  string
	}{
		{"key not read", tableMessagesV2, keyCtx{}, false, liveKeyUndecided},
		{"lookup fails", tableMessagesV2, keyCtx{err: fmt.Errorf("%w: fake", sqlitedb.ErrIndexLimit)}, true, liveKeyUndecided},
		{"undecidable key", tableMessagesV2, keyCtx{err: fmt.Errorf("%w: fake", sqlitedb.ErrKeyUndecidable)}, true, liveKeyUndecided},
		{"short answer, other uid", tableMessagesV2, keyCtx{rows: []fakeRow{keyRow(tableMessagesV2, mid, 1000003)}, lossy: true}, true, liveKeyUndecided},
		{"short answer, no row", tableMessagesV2, keyCtx{lossy: true}, true, liveKeyUndecided},
		{"short answer, equal uid", tableMessagesV2, keyCtx{rows: []fakeRow{keyRow(tableMessagesV2, mid, uid)}, lossy: true}, true, liveKeyPresent},
		{"equal uid", tableMessagesV2, keyCtx{rows: []fakeRow{keyRow(tableMessagesV2, mid, 1000003), keyRow(tableMessagesV2, mid, uid)}}, true, liveKeyPresent},
		{"decided miss, other uid", tableMessagesV2, keyCtx{rows: []fakeRow{keyRow(tableMessagesV2, mid, 1000003)}}, true, liveKeyAbsent},
		{"decided miss", tableMessagesV2, keyCtx{}, true, liveKeyAbsent},
		{"v1: mid alone, other uid", tableMessages, keyCtx{rows: []fakeRow{keyRow(tableMessages, mid, 1000003)}}, true, liveKeyPresent},
		{"v1: decided miss", tableMessages, keyCtx{}, true, liveKeyAbsent},
	} {
		c.ctx.notes = map[string]string{}
		if got := liveKeyState(&c.ctx, nil, c.table, mid, uid, c.ok); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		note, has := c.ctx.notes["live_key_undecided."+c.table+".mid"]
		if failed := c.ctx.err != nil && !c.ctx.lossy && c.name == "lookup fails"; failed != has {
			t.Errorf("%s: note %q (present %v), want present %v", c.name, note, has, failed)
		} else if failed && note == "" {
			t.Errorf("%s: the note names no cause", c.name)
		}
	}
}
