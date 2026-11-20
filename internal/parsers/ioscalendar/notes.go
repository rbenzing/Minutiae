package ioscalendar

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The notes of the store (spec 8.2 "Notes", 6.1, 6.4, Q6, the plan's "Notes and warnings").
const (
	tableOccurrences = "OccurrenceCache" // the occurrence cache (from memory): counted, never a record source (spec 6.1)
	changesSuffix    = "Changes"         // the change-tracking tables (from memory, Q6): counted, never read
	noteUnreadable   = "unreadable"
	// maxChangeTables bounds the change-tracking tables counted (one census of census.MaxSpecs tables); more are named
	// by the trailing ",…" of the note.
	maxChangeTables = census.MaxSpecs
)

// mappedTables are the tables the mapping reads (spec 8.1, 8.2); every other base table is named in the note
// unmapped_tables. A fresh slice per call.
func mappedTables() []string {
	return []string{tableItem, tableLocation, tableParticipant, tableIdentity, tableCalendar, tableStore, tableRecurrence, tableExceptions, tableAlarm}
}

// joinColumns are the columns each join table's join reads or names (its key and owner columns included), for the note
// unmapped_columns.<table> in rowrun's form.
func joinColumns(table string) []string {
	switch table {
	case tableLocation:
		return []string{"title", "address", "latitude", "longitude"}
	case tableCalendar:
		return []string{"title", "store_id"}
	case tableStore:
		return []string{"name", "type"}
	case tableParticipant:
		return []string{"item_id", "identity_id", "email", "role", "type", "status"}
	case tableIdentity:
		return []string{"display_name", "address"}
	case tableRecurrence:
		return append([]string{"owner_id"}, recurrenceCols()...)
	case tableExceptions:
		return []string{"owner_id", "date"}
	case tableAlarm:
		return append([]string{"calendaritem_owner_id"}, alarmCols()...)
	}
	return nil
}

// writeNotes states the store before rowrun.Run (so the host's note cap keeps them): tables (the base tables seen),
// unmapped_tables (those the mapping never reads), occurrence_cache_rows (the row count of a present OccurrenceCache, 0
// included), change_tables ("<table>:<rows>" of the tables whose names end in Changes, Q6), <table> = "unreadable" for a
// counted table the census could not read whole (never "empty", spec 9), and the join tables' unmapped columns. A
// schema read in part adds tables_state = "incomplete": the lists then name the tables seen, and a table not seen is
// never claimed absent.
func writeNotes(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter) error {
	counted := append([]string{tableOccurrences}, joinTableNames()...)
	first := min(len(counted), census.MaxSpecs)
	rep, err := census.Take(ctx, f, b, countSpecs(counted[:first]))
	if err != nil {
		return err
	}
	counts := rep.Counts
	var tables, unmapped, changes []string
	for _, o := range rep.Objects {
		if o.Type == "table" {
			tables = append(tables, o.Name)
		}
	}
	slices.Sort(tables)
	tables = slices.Compact(tables)
	for _, t := range tables {
		if !slices.ContainsFunc(mappedTables(), func(m string) bool { return asciiEqualFold(m, t) }) {
			unmapped = append(unmapped, t)
		}
		if strings.HasSuffix(t, changesSuffix) && len(changes) <= maxChangeTables {
			changes = append(changes, t)
		}
	}
	rest := append(slices.Clone(counted[first:]), changes[:min(len(changes), maxChangeTables)]...)
	for len(rest) > 0 {
		n := min(len(rest), census.MaxSpecs)
		more, err := census.Take(ctx, f, b, countSpecs(rest[:n]))
		if err != nil {
			return err
		}
		counts = append(counts, more.Counts...)
		rest = rest[n:]
	}
	if s := noteList(tables); s != "" {
		out.Note("tables", s)
	}
	if rep.SchemaSkipped > 0 {
		out.Note("tables_state", "incomplete")
	}
	if s := noteList(unmapped); s != "" {
		out.Note("unmapped_tables", s)
	}
	var changeRows []string
	for _, c := range counts {
		switch {
		case c.Unreadable != "":
			out.Note(c.Table, noteUnreadable)
		case !c.Present:
		case c.Table == tableOccurrences:
			out.Note("occurrence_cache_rows", strconv.FormatInt(c.Rows, 10))
		case slices.Contains(changes, c.Table):
			changeRows = append(changeRows, c.Table+":"+strconv.FormatInt(c.Rows, 10))
		}
	}
	s := noteList(changeRows)
	if len(changes) > maxChangeTables && !strings.HasSuffix(s, noteCutMark) {
		s += noteCutMark // more change tables than were counted
	}
	if s != "" {
		out.Note("change_tables", s)
	}
	return joinColumnNotes(ctx, f, b, out)
}

// joinTableNames are the names of joinTables, in their order. A fresh slice per call.
func joinTableNames() []string {
	var out []string
	for _, jt := range joinTables() {
		out = append(out, jt.Table)
	}
	return out
}

func countSpecs(tables []string) []census.CountSpec {
	specs := make([]census.CountSpec, len(tables))
	for i, t := range tables {
		specs[i] = census.CountSpec{Table: t}
	}
	return specs
}

// joinColumnNotes states, in the form of rowrun's unmapped_columns.<table>, the columns of each join table that its
// join neither reads nor names, the rowid alias excepted. A table that is absent or cannot be read states nothing here
// (its join states it).
func joinColumnNotes(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter) error {
	db, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		return err
	}
	defer db.Release()
	for _, table := range joinTableNames() {
		t, err := db.Table(ctx, table, nil, nil)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable), err != nil && dataDamage(err):
			continue
		case err != nil:
			return err
		}
		known := joinColumns(table)
		var unmapped []string
		for _, c := range t.Cols() {
			if !c.RowidAlias && !slices.ContainsFunc(known, func(k string) bool { return asciiEqualFold(k, c.Name) }) {
				unmapped = append(unmapped, c.Name)
			}
		}
		if len(unmapped) > 0 {
			out.Note("unmapped_columns."+table, noteList(unmapped))
		}
	}
	return nil
}
