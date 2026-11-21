package androidtelegram

import (
	"context"
	"maps"
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
	maxNote         = 512  // the bound of every list note but constructors_seen (F11)
	maxSeenNote     = 1000 // the bound of constructors_seen: 64 entries and "other" fit under the host's 1 KiB (F11)
	maxSeenEntries  = 64   // the top constructors named one by one in constructors_seen
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
func noteList(names []string, limit int) string { return noteListSep(names, limit, ",") }

// noteListSep is noteList with the entries joined by sep (blob_states joins its tables with ";").
func noteListSep(names []string, limit int, sep string) string {
	var sb strings.Builder
	for _, n := range names {
		add := len(n)
		if sb.Len() > 0 {
			add += len(sep)
		}
		if sb.Len()+add > limit-len(noteListCutMark) {
			sb.WriteString(noteListCutMark)
			break
		}
		if sb.Len() > 0 {
			sb.WriteString(sep)
		}
		sb.WriteString(n)
	}
	return sb.String()
}

// writeCountNotes writes the notes of what the run counted (spec 8.4, 8.10, the plan's notes table): blob_states
// ("<table>.<col>=<state>:<n>,…" per TL column in table order, the states in vocabulary order, the columns joined by
// ";", cut at 512 bytes) and constructors_seen (the message blobs' top IDs "0x<id>:<n>" by count descending then ID,
// at most 64, then "other:<n>", cut at 1,000 bytes, F11) and secret_chats (secretChatsNote). They follow rowrun.Run: what they count exists only then.
// secretChatsNote is the note secret_chats (spec 8.10, M4): the distinct secret chats among the message records, by
// their enc_chats key; when some records' dialog is undecided (an unreadable uid), a lower bound that says how many,
// never a definite count: ">=<n> (<k> rows with an undecided dialog)".
func secretChatsNote(c *countingEmitter) string {
	n := strconv.Itoa(len(c.secret))
	if c.undec == 0 {
		return n
	}
	return ">=" + n + " (" + strconv.FormatInt(c.undec, 10) + " rows with an undecided dialog)"
}

func writeCountNotes(out parse.Emitter, c *countingEmitter) {
	var states []string
	for _, table := range countedTables() {
		for _, col := range blobColumns() {
			counts := c.states[table+"."+col]
			var parts []string
			for _, st := range blobStates() {
				if counts[st] > 0 {
					parts = append(parts, st+":"+strconv.FormatInt(counts[st], 10))
				}
			}
			if len(parts) > 0 {
				states = append(states, table+"."+col+"="+strings.Join(parts, ","))
			}
		}
	}
	if len(states) > 0 {
		out.Note("blob_states", noteListSep(states, maxNote, ";"))
	}
	out.Note("secret_chats", secretChatsNote(c))
	if len(c.seen) == 0 {
		return
	}
	ids := slices.Collect(maps.Keys(c.seen))
	slices.SortFunc(ids, func(a, b string) int {
		switch {
		case c.seen[a] > c.seen[b]:
			return -1
		case c.seen[a] < c.seen[b]:
			return 1
		}
		return strings.Compare(a, b) // 0x and 8 lower-case hex digits: byte order is ID order
	})
	var entries []string
	var other int64
	for i, id := range ids {
		if i < maxSeenEntries {
			entries = append(entries, id+":"+strconv.FormatInt(c.seen[id], 10))
			continue
		}
		other += c.seen[id]
	}
	if other > 0 {
		entries = append(entries, "other:"+strconv.FormatInt(other, 10))
	}
	out.Note("constructors_seen", noteList(entries, maxSeenNote))
}
