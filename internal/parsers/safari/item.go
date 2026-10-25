package safari

import (
	"context"
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

const browser = "safari"

func itemSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "url", Required: true, Affinity: "text"},
		{Name: "visit_count", Affinity: "integer"},
		{Name: "status_code", Affinity: "integer"},
	}
}

func itemsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "history_items", Columns: itemSpecs(), Map: mapItem}
}

func itemColumns() []string { return []string{"id", "url", "visit_count", "status_code"} }

// fatal reports the errors a mapping hands back to the runner (the job fails, loudly): a budget, a cancelled
// or expired context and a failed mapping Context. Every other error of a join is about that join.
func fatal(err error) bool {
	return errors.Is(err, parse.ErrBudget) || errors.Is(err, sqlitefile.ErrBudget) || errors.Is(err, rowrun.ErrContext) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// itemFacts states visit_count and status_code of a history_items row (4F-R8, R24). prefix is "" for the
// item's own record (the flags go to unknown_columns) and "history_items." for the item joined to a visit
// (the flags go to joined_unknown_columns, where a column the variant lacks is named too).
func itemFacts(row parse.Row, c rowrun.Cols, v *web.Visit, raw, flags map[string]any, prefix string) {
	flag := func(name string, k rowrun.CellKind) {
		if k == rowrun.CellMissing {
			if prefix != "" {
				flags[prefix+name] = "column-missing"
			}
			return
		}
		rowrun.Flag(flags, row, prefix+name, c.At(name), k)
	}
	n, k := rowrun.IntCell(row, c.At("visit_count"))
	switch {
	case k == rowrun.CellValue && n >= 0:
		v.URLVisitCount = &n
	case k == rowrun.CellValue:
		flags[prefix+"visit_count"] = "negative"
	default:
		flag("visit_count", k)
	}
	s, k := rowrun.IntCell(row, c.At("status_code"))
	switch {
	case k == rowrun.CellValue && s != 0:
		v.StatusCode = &s
	case k == rowrun.CellValue:
		raw["status_code"] = int64(0) // not recorded, stated as stored
	default:
		flag("status_code", k)
	}
}

// itemURL reads the usable URL of a history_items row: non-empty text.
func itemURL(row parse.Row, c rowrun.Cols) ([]byte, bool) {
	b, k := rowrun.TextCell(row, c.At("url"))
	return b, k == rowrun.CellValue && len(b) > 0
}

// finish completes a record: the flags, the damage of the row, the typed payload, body and summary.
func finish(rec records.Record, v web.Visit, raw, unknown, joined map[string]any, row parse.Row, title string) records.Record {
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if len(joined) > 0 {
		raw["joined_unknown_columns"] = joined
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Body = bodyOf(raw, v.URL, title)
	v.Raw = raw
	rec.Summary = web.VisitSummary(v)
	rec.Payload = v.Payload()
	return rec
}

// mapItem maps a history_items row: only an item without any visit gives a record (4F-R9). A visit's
// URL comes from its item, so an item with visits gives nothing here.
func mapItem(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "history_items", itemColumns()...)
	raw, unknown := map[string]any{}, map[string]any{}
	v := web.Visit{Browser: browser}

	// The visits join is information, not a requirement: an answer the engine cannot give is stated as
	// undecided, never as "no visits" and never as "has visits" (4F-R9, R22).
	undecided := true
	if id, k := rowrun.IntCell(row, c.At("id")); k == rowrun.CellValue {
		rows, und, err := rowrun.Match(m, "history_visits", "history_item", id)
		switch {
		case err != nil && fatal(err):
			return nil, err
		case err != nil:
		case len(rows) > 0:
			return nil, nil
		default:
			undecided = und
		}
	}
	if undecided {
		raw["join_undecided"] = []any{"history_visits"}
	} else {
		raw["visitless"] = true
	}

	urlb, ok := itemURL(row, c) // after the join: an item with visits is reported by its visits
	if !ok {
		return nil, rowrun.Unmappable("history_items.url holds no usable text")
	}
	v.URL = cleanText(raw, "url", urlb, maxURL)
	itemFacts(row, c, &v, raw, unknown, "")
	rec := records.Record{Type: web.VisitType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	return []records.Record{finish(rec, v, raw, unknown, nil, row, "")}, nil
}

// getItem finds the history_items row a visit names, by rowid (4F-R30). A clean miss, a table or schema the
// engine cannot read and an id cell that is not the rowid are RowErrors; the budget and cancellation return.
func getItem(m parse.MapContext, key int64) (parse.Row, rowrun.Cols, error) {
	row, found, err := m.Get("history_items", key)
	switch {
	case err != nil && fatal(err):
		return nil, nil, err
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return nil, nil, rowrun.Unmappable("history_item names a row of history_items, which the database lacks")
	case err != nil:
		return nil, nil, rowrun.Unmappable("history_items cannot be read for history_item")
	case !found:
		return nil, nil, rowrun.Unmappable("history_item names no history_items row")
	}
	c := rowrun.Resolve(m, "history_items", itemColumns()...)
	if id, k := rowrun.IntCell(row, c.At("id")); k != rowrun.CellValue || id != key {
		return nil, nil, rowrun.Unmappable("history_items.id is not the rowid, so history_item cannot be joined by rowid")
	}
	return row, c, nil
}
