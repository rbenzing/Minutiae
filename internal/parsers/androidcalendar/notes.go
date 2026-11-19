package androidcalendar

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The notes of the store (spec 7.3, the plan's "Notes and warnings"): a value is cut at maxNote bytes, a list at an
// entry boundary with a trailing ",…", a text with a trailing "…".
const (
	maxNote         = 512
	tableInstances  = "Instances"
	tableMetaData   = "CalendarMetaData"
	tableCache      = "CalendarCache"
	noteUnreadable  = "unreadable"
	noteListCutMark = ",…"
)

// unmappedTableNames are the tables of calendar.db the parser never reads (the plan's "Notes and warnings"), in the
// order the note names them: the Instances cache of expanded occurrences is not a record source (spec 6.1, Q8).
var unmappedTableNames = []string{tableInstances, "Colors", "_sync_state", "_sync_state_metadata"}

// censusTables are the tables the census counts: Instances (its row count is a note) and the join tables, so that a
// table that cannot be read whole is the note <table> = "unreadable", never "empty" (spec 9).
var censusTables = []string{tableInstances, tableCalendars, tableAttendees, tableReminders, tableAlerts, tableProperties, tableRawTimes}

// The CalendarMetaData columns and the CalendarCache keys stated as notes, as stored and never applied (spec 7.3).
var (
	metaCols  = []string{"localTimezone", "minInstance", "maxInstance"}
	cacheKeys = []string{"timezoneType", "timezoneInstances", "timezoneInstancesPrevious"}
)

// writeNotes states the store before rowrun.Run (so the host's note cap keeps them): database_version (the live
// view's user version, the only owner of that note, F08), tables (the base tables present), unmapped_tables,
// rows.Instances, <table> = "unreadable" for a counted table the census could not read whole, and the metadata and
// cache values of the provider as stored.
func writeNotes(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter, version int32) error {
	out.Note("database_version", strconv.FormatInt(int64(version), 10))
	specs := make([]census.CountSpec, len(censusTables))
	for i, t := range censusTables {
		specs[i] = census.CountSpec{Table: t}
	}
	rep, err := census.Take(ctx, f, b, specs)
	if err != nil {
		return err
	}
	var tables, unmapped []string
	for _, o := range rep.Objects {
		if o.Type == "table" {
			tables = append(tables, o.Name)
		}
	}
	slices.Sort(tables)
	if s := noteList(slices.Compact(tables)); s != "" {
		out.Note("tables", s)
	}
	for _, want := range unmappedTableNames {
		if slices.ContainsFunc(tables, func(n string) bool { return asciiEqualFold(n, want) }) {
			unmapped = append(unmapped, want)
		}
	}
	if s := noteList(unmapped); s != "" {
		out.Note("unmapped_tables", s)
	}
	for _, c := range rep.Counts {
		switch {
		case c.Unreadable != "":
			out.Note(c.Table, noteUnreadable)
		case c.Present && c.Table == tableInstances:
			out.Note("rows."+tableInstances, strconv.FormatInt(c.Rows, 10))
		}
	}
	return storeValues(ctx, f, b, out)
}

// noteList is the comma list of names cut at an entry boundary within maxNote bytes, with a trailing ",…".
func noteList(names []string) string {
	var sb strings.Builder
	for _, n := range names {
		add := len(n)
		if sb.Len() > 0 {
			add++
		}
		if sb.Len()+add > maxNote-len(noteListCutMark) {
			sb.WriteString(noteListCutMark)
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(n)
	}
	return sb.String()
}

// storeValues states the first CalendarMetaData row's metaCols as meta.<col> and the CalendarCache values of
// cacheKeys as cache.<key>, each as stored (never applied). A table the database lacks states nothing; one that
// cannot be read whole (a damaged page met while scanning it) is the note <table> = "unreadable".
func storeValues(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter) error {
	db, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		return err
	}
	defer db.Release()
	scan := func(table string, fn func(t *sqlitedb.Table, row sqlitedb.Row) error) error {
		t, err := db.Table(ctx, table, nil, nil)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			return nil
		case err != nil && dataDamage(err):
			out.Note(table, noteUnreadable)
			return nil
		case err != nil:
			return err
		}
		before := len(db.Warnings())
		err = t.Scan(ctx, func(row sqlitedb.Row) error { return fn(t, row) })
		switch {
		case err != nil && dataDamage(err), err == nil && len(db.Warnings()) > before:
			out.Note(table, noteUnreadable)
			return nil
		}
		return err
	}
	err = scan(tableMetaData, func(t *sqlitedb.Table, row sqlitedb.Row) error {
		for _, c := range metaCols {
			if v, ok := noteValue(row, t.Col(c)); ok {
				out.Note("meta."+c, v)
			}
		}
		return sqlitedb.ErrStop // the first row only
	})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	return scan(tableCache, func(t *sqlitedb.Table, row sqlitedb.Row) error {
		k, ok := row.Text(t.Col("key"))
		if !ok || !slices.Contains(cacheKeys, string(k)) || seen[string(k)] {
			return nil
		}
		if v, ok := noteValue(row, t.Col("value")); ok {
			seen[string(k)] = true
			out.Note("cache."+string(k), v)
		}
		return nil
	})
}

// noteValue is a stored cell as a note value: TEXT cleaned and cut at maxNote bytes (a cut one ends in "…"), an
// INTEGER or a finite REAL in decimal, a BLOB as blob_b64:<base64> (cut likewise); an unreadable cell is
// "unreadable"; NULL and a missing column state nothing.
func noteValue(row sqlitedb.Row, col int) (string, bool) {
	if col < 0 {
		return "", false
	}
	if !row.State(col).Known() {
		return noteUnreadable, true
	}
	if row.IsNull(col) {
		return "", false
	}
	if v, ok := row.Int(col); ok {
		return strconv.FormatInt(v, 10), true
	}
	if b, ok := row.Text(col); ok {
		return noteText(b), true
	}
	if f, ok := storedOther(row, col); ok {
		switch v := f.(type) {
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64), true
		case map[string]any:
			s, _ := v["blob_b64"].(string)
			return noteText([]byte("blob_b64:" + s)), true
		}
	}
	return noteUnreadable, true
}

// noteText cleans b and cuts it within maxNote bytes, a cut text ending in "…".
func noteText(b []byte) string {
	c := common.CleanText(b, maxNote-len("…"))
	if c.Truncated {
		return c.Text + "…"
	}
	return c.Text
}

// writeCountNotes states, after rowrun.Run, the records Deleted by Events.deleted (soft_deleted) and by
// Events.lastSynced (lastsynced_copies), from the counting emitter (one owner per note, F08). A zero count states
// nothing. No count of the events of a deleted calendar is stated: spec 7.3 lists none.
func writeCountNotes(out parse.Emitter, c counts) {
	if c.SoftDeleted > 0 {
		out.Note("soft_deleted", strconv.FormatInt(c.SoftDeleted, 10))
	}
	if c.LastSynced > 0 {
		out.Note("lastsynced_copies", strconv.FormatInt(c.LastSynced, 10))
	}
}
