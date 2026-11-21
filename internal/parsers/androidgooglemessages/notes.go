package androidgooglemessages

import (
	"context"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The notes of the store (spec 5.4, the plan's "Notes and warnings"): a list is cut at an entry boundary within its
// bound with a trailing ",…".
const (
	maxNote         = 512
	noteUnreadable  = "unreadable"
	noteListCutMark = ",…"
)

// countedTables are the five core tables, in the order of the notes rows.<table> and columns_missing.<table>.
func countedTables() []string {
	return []string{tableMessages, tableParts, tableParticipants, tableConversations, tableMembers}
}

// writeNotes states the store before rowrun.Run (so the host's note cap keeps them): user_version and whether it is
// cited, journal_mode, app and database_user, rows.<table> for every counted table present, <table> = "unreadable" for
// a counted table the census could not read whole (never "empty"; it also sets lv.MessagesUnreadable), and
// columns_missing.<table> (B4, once per job).
func writeNotes(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, out parse.Emitter, lv *liveView, app, user string) error {
	out.Note("user_version", strconv.FormatInt(int64(lv.Version), 10))
	cited := "no"
	if lv.Version == 1 || lv.Version == 2 {
		cited = strconv.FormatInt(int64(lv.Version), 10)
	}
	out.Note("user_version_cited", cited)
	out.Note("journal_mode", lv.JournalMode)
	out.Note("app", app)
	out.Note("database_user", user)
	tables := countedTables()
	specs := make([]census.CountSpec, len(tables))
	for i, t := range tables {
		specs[i] = census.CountSpec{Table: t}
	}
	rep, err := census.Take(ctx, f, b, specs)
	if err != nil {
		return err
	}
	for _, c := range rep.Counts {
		switch {
		case c.Unreadable != "":
			out.Note(c.Table, noteUnreadable)
			if c.Table == tableMessages {
				lv.MessagesUnreadable = true
			}
		case c.Present:
			out.Note("rows."+c.Table, strconv.FormatInt(c.Rows, 10))
		}
	}
	for _, t := range tables {
		if cols := lv.Missing[t]; len(cols) > 0 {
			out.Note("columns_missing."+t, noteList(cols, maxNote))
		}
	}
	return nil
}

// noteList is the comma list of names cut at an entry boundary within limit bytes, with a trailing ",…" (restated from
// androidtelegram/notes.go: parsers import no other parser).
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
