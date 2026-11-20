package androidtelegram

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The notes of the store (spec 8.10, the plan's "Notes and warnings"): a list is cut at an entry boundary within its
// bound with a trailing ",…".
const (
	maxNote         = 512 // the bound of every list note but constructors_seen (F11)
	noteUnreadable  = "unreadable"
	noteListCutMark = ",…"
)

// The values of the note generation: the message tables holding rows.
const (
	generationBoth      = "both"
	generationNone      = "none"
	generationUndecided = "undecided" // a message table whose rows could not be counted whole and gave none
)

// rowNoteTables are the tables whose census row count is the note rows.<table> (each when present), in the order of the
// plan's notes table (spec 8.10, Q10). enc_chats is counted only (spec 9).
func rowNoteTables() []string {
	return []string{
		tableMessages, tableMessagesV2, tableUsers, tableChats, tableEncChats, tableContacts, tableUsersData, "media_v4",
		"media_v3", "media_v2", "dialogs", "messages_topics", "scheduled_messages_v2", "user_contacts_v7",
	}
}

// readTables are the seven tables of spec 6.3 the parser reads; every other table is named in unmapped_tables.
func readTables() []string {
	return []string{tableMessages, tableMessagesV2, tableUsers, tableUsersData, tableChats, tableContacts, tableEncChats}
}

// writeNotes states the store before rowrun.Run (so the host's note cap keeps them): database_version (the live view's
// user version), account and database_user (from the path, 4I4-P7), generation (the message tables holding rows),
// rows.<table> for every counted table present, <table> = "unreadable" for a counted table the census could not read
// whole (never "empty"), and unmapped_tables (the tables never read, in byte order).
func writeNotes(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter, lv liveView, account, user string) error {
	out.Note("database_version", strconv.FormatInt(int64(lv.Version), 10))
	out.Note("account", account)
	out.Note("database_user", user)
	rep, err := countTables(ctx, f, b, rowNoteTables())
	if err != nil {
		return err
	}
	held := map[string]string{} // message table -> "rows", "none" or "unknown"
	for _, c := range rep.Counts {
		switch {
		case c.Present && c.Rows > 0:
			held[c.Table] = "rows" // rows were seen, even if the scan then stopped
		case c.Unreadable != "":
			held[c.Table] = "unknown"
		default:
			held[c.Table] = "none"
		}
		switch {
		case c.Unreadable != "":
			out.Note(c.Table, noteUnreadable)
		case c.Present:
			out.Note("rows."+c.Table, strconv.FormatInt(c.Rows, 10))
		}
	}
	out.Note("generation", generation(held[tableMessages], held[tableMessagesV2]))
	var unmapped []string
	for _, o := range rep.Objects {
		if o.Type == "table" && !slices.ContainsFunc(readTables(), func(n string) bool { return asciiEqualFold(n, o.Name) }) {
			unmapped = append(unmapped, o.Name)
		}
	}
	slices.Sort(unmapped)
	if s := noteList(slices.Compact(unmapped), maxNote); s != "" {
		out.Note("unmapped_tables", s)
	}
	return nil
}

// countTables takes the census of the tables, census.MaxSpecs at a time (one Take accepts no more): the schema and
// its damage are those of the first Take, the counts those of every Take in order.
func countTables(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, tables []string) (census.Report, error) {
	var rep census.Report
	for start := 0; start == 0 || start < len(tables); start += census.MaxSpecs {
		chunk := tables[start:min(start+census.MaxSpecs, len(tables))]
		specs := make([]census.CountSpec, len(chunk))
		for i, t := range chunk {
			specs[i] = census.CountSpec{Table: t}
		}
		r, err := census.Take(ctx, f, b, specs)
		if err != nil {
			return census.Report{}, err
		}
		if start == 0 {
			rep.Objects, rep.SchemaSkipped = r.Objects, r.SchemaSkipped
		}
		rep.Counts = append(rep.Counts, r.Counts...)
	}
	return rep, nil
}

// generation names the message tables holding rows (spec 8.3): a table whose count is unknown and gave no row leaves
// the generation undecided, never "none".
func generation(v1, v2 string) string {
	switch {
	case v1 == "unknown" || v2 == "unknown":
		return generationUndecided
	case v1 == "rows" && v2 == "rows":
		return generationBoth
	case v1 == "rows":
		return tableMessages
	case v2 == "rows":
		return tableMessagesV2
	}
	return generationNone
}

// noteList is the comma list of names cut at an entry boundary within limit bytes, with a trailing ",…" (restated from
// androidcalendar/notes.go with the cut as a parameter, F11, F29).
func noteList(names []string, limit int) string {
	var sb strings.Builder
	for _, n := range names {
		add := len(n)
		if sb.Len() > 0 {
			add++
		}
		if sb.Len()+add > limit-len(noteListCutMark) {
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
