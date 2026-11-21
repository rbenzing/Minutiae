package androidtelegram

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Typed recovery (spec 11, call 7, ruling T2): android.telegram claims the tables whose mapping is total
// (ClaimableTables), so the host's typed pass, android.telegram.recover, maps their recovered rows with a nil env. A
// recovered row decodes its own plain columns and blobs as a live row does and joins no table: every join-derived
// state is "not-joined-recovered" (B2, 4I4-P6). Its relation to the live table is the host's (absent-from-live,
// superseded-version) and, because the app re-inserts a message on every put (putReinserts), the row's own key (mid,
// uid) read against the live rows of its own table: raw.live_key_state.

// The states of raw.live_key_state (rulings T2, T9, 4I4-P6): present (a live row of the same table has the recovered
// row's key: the message's key is still live and the row is an older image of it; the host's Deleted stamp on a typed
// record marks recovered provenance, T8), absent (a complete answer holds no live row with that key) and undecided
// (the row's own key, the live rows or the lookup could not be read or decided).
const (
	liveKeyPresent   = "present"
	liveKeyAbsent    = "absent"
	liveKeyUndecided = stateUndecided
)

// claimsOf are the claims of tables on the db role (restated from androidcalendar, F29).
func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// liveKeyState is the relation of a recovered message row to the live rows of its own table by its own primary key
// (ruling T2, 4I4-P6): (mid, uid) in messages_v2; mid alone in messages, whose mid is the rowid that the pre-84 put
// (REPLACE INTO on mid) replaces (ruling T9). ok says the key cells were read as integers. The live rows with that mid
// are matched under the engine's equality (rowrun.Match) and, in messages_v2, each one's uid is compared: an equal key
// is present, even on a short answer (the row that was found is live); no equal key on a complete answer, with every
// live uid read, is absent. A key that was not read, a live uid that cannot be read, a short answer (an index that
// lost rows, an undecidable key) without the key, or a lookup that fails is undecided, never absent: the same mid in
// another dialog never makes a recovered row present (TestTelegramSameMidInTwoDialogsIsNotMerged). A failed lookup
// (the mid index could not be built: its cap, the budget, damage) is stated once per run in the note
// live_key_undecided.<table>.mid with its cause (the host keeps the first note of a key), so a run of undecided
// states has a stated reason. This is identity from the row's own key in the same table, not a join.
func liveKeyState(m parse.MapContext, once map[string]bool, table string, mid, uid int64, ok bool) string {
	if !ok {
		return liveKeyUndecided
	}
	rows, short, err := rowrun.Match(m, table, "mid", mid)
	if err != nil {
		if key := "live_key_undecided." + table + ".mid"; !once[key] { // built once per table per run, not per recovered row
			if once != nil {
				once[key] = true
			}
			m.Note(key, "the live "+table+".mid lookup failed: "+err.Error())
		}
		return liveKeyUndecided
	}
	if table == tableMessages && len(rows) > 0 {
		return liveKeyPresent // the key is mid alone (T9)
	}
	col := m.Column(table, "uid")
	undecided := short
	for _, r := range rows {
		v, k := intCellOf(r, col)
		switch {
		case k == rowrun.CellValue && v == uid:
			return liveKeyPresent
		case k != rowrun.CellValue && k != rowrun.CellNull:
			undecided = true // a live uid of another class or unreadable might be the key: never absent
		}
	}
	if undecided {
		return liveKeyUndecided
	}
	return liveKeyAbsent
}
