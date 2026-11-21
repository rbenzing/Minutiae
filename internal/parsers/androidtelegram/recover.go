package androidtelegram

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Typed recovery (spec 11, call 7, ruling T2): android.telegram claims the tables whose mapping is total
// (ClaimableTables), so the host's typed pass, android.telegram.recover, maps their recovered rows with a nil env. A
// recovered row decodes its own plain columns and blobs as a live row does and joins no table: every join-derived
// state is "not-joined-recovered" (B2, 4I4-P6). Its relation to the live table is the host's (absent-from-live,
// superseded-version) and, because the app re-inserts a message on every put (putReinserts), the row's own key (mid,
// uid) read against the live rows of its own table: raw.live_key_state.

// The states of raw.live_key_state (ruling T2, 4I4-P6): present (a live row of the same table has the recovered row's
// mid and uid: the recovered row is an older image of a message still live, never a deletion), absent (no live row
// has that key) and undecided (the row's own key, or the live table, could not be read).
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
// (mid, uid) (ruling T2, 4I4-P6): ok says both cells were read as integers. The live rows with that mid are matched
// under the engine's equality and each one's uid is compared: one equal uid is present; none, with every uid read, is
// absent. A key that was not read, a live uid that cannot be read, or a lookup that fails (the table absent or
// damaged, an undecidable index, the budget) is undecided, never absent: the same mid in another dialog never makes a
// recovered row present (TestTelegramSameMidInTwoDialogsIsNotMerged). This is identity from the row's own key in the
// same table, not a join.
func liveKeyState(m parse.MapContext, table string, mid, uid int64, ok bool) string {
	if !ok {
		return liveKeyUndecided
	}
	rows, err := m.Match(table, "mid", sqlitedb.LiteralIntKey(mid))
	if err != nil {
		return liveKeyUndecided
	}
	col := m.Column(table, "uid")
	undecided := false
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
