package androidcalendar

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The deletion sources (spec 6.3, B7) in their order of precedence, and the relation of a last-synced copy (B3).
const (
	srcEventsDeleted   = "Events.deleted"
	srcLastSynced      = "Events.lastSynced"
	srcCalendarDeleted = "Calendars.deleted"
	relationLastSynced = "last-synced-copy"
)

// The states of a flag: "" a decided 0 or 1 in Value; flagAbsent no such flag for this row (a column the variant
// lacks, or no calendar row); flagUndecided a NULL, another value or an unreadable cell; notJoinedRecovered a
// calendar flag of a recovered row, which joins nothing (B2, 4I3-P4).
const (
	flagAbsent         = "absent"
	flagUndecided      = "undecided"
	notJoinedRecovered = "not-joined-recovered"
)

// flag is one deletion or sync flag of a row.
type flag struct {
	Value int64
	State string
}

func (f flag) set() bool { return f.State == "" && f.Value == 1 }

// deletion is what the flags of a row decide (B7): Deleted with the first matching source (and its relation), the
// other matching sources in Also, and in States the raw state keys of the flags that could not be decided.
type deletion struct {
	Deleted          bool
	Source, Relation string
	Also             []string
	States           map[string]string
}

// deletionOf applies B7: Events.deleted, then Events.lastSynced, then Calendars.deleted. An undecided flag is
// stated (deleted_state, lastSynced_state, deleted_via_state) and never decides a deletion; it never makes a row
// live either: the row is simply not claimed deleted by that flag.
func deletionOf(evDeleted, lastSynced, calDeleted flag) deletion {
	d := deletion{States: map[string]string{}}
	var matches []string
	for _, f := range []struct {
		f        flag
		src, key string
	}{
		{evDeleted, srcEventsDeleted, "deleted_state"},
		{lastSynced, srcLastSynced, "lastSynced_state"},
		{calDeleted, srcCalendarDeleted, "deleted_via_state"},
	} {
		switch {
		case f.f.State == flagUndecided || f.f.State == notJoinedRecovered:
			d.States[f.key] = f.f.State
		case f.f.set():
			matches = append(matches, f.src)
		}
	}
	if len(matches) == 0 {
		return d
	}
	d.Deleted, d.Source, d.Also = true, matches[0], matches[1:]
	if d.Source == srcLastSynced {
		d.Relation = relationLastSynced
	}
	return d
}

// record writes the decision into the payload's deleted object (nil when not deleted) and raw.
func (d deletion) record(raw map[string]any) map[string]any {
	for k, v := range d.States {
		raw[k] = v
	}
	if !d.Deleted {
		return nil
	}
	del := map[string]any{"source": d.Source}
	if d.Relation != "" {
		del["relation"] = d.Relation
	}
	if d.Source == srcCalendarDeleted {
		raw["deleted_via"] = "calendar"
	}
	if len(d.Also) > 0 {
		raw["deletion_also"] = strings.Join(d.Also, ",")
	}
	return del
}

// readFlag reads a 0/1 flag of a row (spec 6.3): a NULL is undecided, as is a missing column when missingUndecided
// (else absent); an INTEGER other than 0 and 1 is listed as "out-of-set" in unknown_columns, a cell of another class
// as "type-mismatch", an unreadable cell by its decoder state; each of these is undecided.
func readFlag(row parse.Row, col int, name string, unknown map[string]any, missingUndecided bool) flag {
	v, k := intCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		if v == 0 || v == 1 {
			return flag{Value: v}
		}
		unknown[name] = "out-of-set"
	case rowrun.CellMissing:
		if !missingUndecided {
			return flag{State: flagAbsent}
		}
	case rowrun.CellNull:
	default:
		rowrun.Flag(unknown, row, name, col, k)
	}
	return flag{Value: v, State: flagUndecided}
}
