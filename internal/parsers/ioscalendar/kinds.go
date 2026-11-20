package ioscalendar

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The item kinds of CalendarItem.entity_type (from memory, spec 8.2, Q11): 2 is an event, 3 a reminder or task.
const (
	kindEvent = 2
	kindTask  = 3
)

// The row rule's texts (4I3-P10, the plan's state vocabulary).
const (
	reasonNotEvent = "CalendarItem row is not an event (entity_type 3)"
	stateUndecided = "undecided"
)

// The bounds of the kinds note: at most maxKinds distinct values are named (the event and task kinds always among
// them, so non_event_rows is exact), the rest counted as other; the note value is cut at maxNote bytes.
const (
	maxKinds    = 16
	maxNote     = 512
	noteCutMark = ",…"
)

// kinds are the INTEGER values of CalendarItem.entity_type in the live view, by value (at most maxKinds keys), with
// the count of the rows whose value found no key (Other); Absent says the table has no entity_type column. A NULL,
// a cell of another class and an unreadable cell are no value and are not counted.
type kinds struct {
	Values map[int64]int64
	Other  int64
	Absent bool
}

// add counts one value: the event and task kinds always have a key; another value has one while fewer than
// maxKinds-2 other values have one.
func (k *kinds) add(v int64) {
	if _, ok := k.Values[v]; ok || v == kindEvent || v == kindTask {
		k.Values[v]++
		return
	}
	others := len(k.Values)
	for _, r := range []int64{kindEvent, kindTask} {
		if _, ok := k.Values[r]; ok {
			others--
		}
	}
	if others < maxKinds-2 {
		k.Values[v]++
		return
	}
	k.Other++
}

// countKinds reads CalendarItem.entity_type in one Scan of the live view (4I3-P10), bounded by the maxKinds keys.
// The error of a table the view lacks wraps sqlitedb.ErrNoSuchTable.
func countKinds(ctx context.Context, db *sqlitedb.DB) (kinds, error) {
	t, err := db.Table(ctx, tableItem, nil, nil)
	if err != nil {
		return kinds{}, err
	}
	col := t.Col("entity_type")
	if col < 0 {
		return kinds{Absent: true}, nil
	}
	k := kinds{Values: map[int64]int64{}}
	err = t.Scan(ctx, func(row sqlitedb.Row) error {
		if v, kd := intCellOf(row, col); kd == rowrun.CellValue {
			k.add(v)
		}
		return nil
	})
	return k, err
}

// note writes the notes of the kinds (4I3-P10): entity_type = absent when the column is absent; else
// entity_type_values ("<value>:<rows>" by value, then other:<rows>; "none" when no row holds an INTEGER value), and
// non_event_rows (the rows of the task kind, each a warning and no record) when there is one.
func (k kinds) note(out parse.Emitter) {
	if k.Absent {
		out.Note("entity_type", "absent")
		return
	}
	vals := make([]int64, 0, len(k.Values))
	for v := range k.Values {
		vals = append(vals, v)
	}
	slices.Sort(vals)
	entries := make([]string, 0, len(vals)+1)
	for _, v := range vals {
		entries = append(entries, strconv.FormatInt(v, 10)+":"+strconv.FormatInt(k.Values[v], 10))
	}
	if k.Other > 0 {
		entries = append(entries, "other:"+strconv.FormatInt(k.Other, 10))
	}
	if len(entries) == 0 {
		entries = append(entries, "none") // the column exists but no row holds an INTEGER kind (Task 8 review M1)
	}
	out.Note("entity_type_values", noteList(entries))
	if n := k.Values[kindTask]; n > 0 {
		out.Note("non_event_rows", strconv.FormatInt(n, 10))
	}
}

// noteList is the comma list of entries cut at an entry boundary within maxNote bytes, with a trailing ",…".
func noteList(entries []string) string {
	var sb strings.Builder
	for _, e := range entries {
		add := len(e)
		if sb.Len() > 0 {
			add++
		}
		if sb.Len()+add > maxNote-len(noteCutMark) {
			sb.WriteString(noteCutMark)
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(e)
	}
	return sb.String()
}

// itemKind applies the row rule (spec 8.2, Q11, 4I3-P10): entity_type 2 is an event; 3 is no record but a counted
// warning (rowrun.Unmappable); NULL, another value or an unreadable cell is a record whose kind is undecided
// (raw.item_kind_state, the value as stored in raw.entity_type, an unreadable cell in unknown_columns); a missing
// column states nothing (the note entity_type = absent says it).
func itemKind(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) error {
	col := c.At("entity_type")
	v, k := intCellOf(row, col)
	switch k {
	case rowrun.CellMissing:
		return nil
	case rowrun.CellValue:
		switch v {
		case kindEvent:
			return nil
		case kindTask:
			return rowrun.Unmappable(reasonNotEvent)
		}
		raw["entity_type"] = v
	case rowrun.CellNull:
		raw["entity_type"] = nil
	default:
		flag(unknown, row, unknownPrefix+"entity_type", col, k)
	}
	raw["item_kind_state"] = stateUndecided
	return nil
}
