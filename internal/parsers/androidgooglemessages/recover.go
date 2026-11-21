package androidgooglemessages

import (
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Typed recovery (spec 8, C5, C5a, B1): android.googlemessages claims messages and parts, whose mappings are total, so the
// host's typed pass maps their recovered rows with a nil env. A recovered messages row is the spec 6 mapping of its own
// columns and joins nothing. A recovered parts row is a record of its own; it joins the live message its message_id names
// only by structure (spec 8.3) and then copies that message's channel, direction and participants, never merged into the
// live record (B1). The host stamps the recovery provenance (Deleted, payload.recovery, the relation): the mapping never
// states a deletion and never presents a part as live.

// ClaimableTables are the tables whose mapping is total (spec 8.1): every recovered messages or parts row is one record or a
// counted warning. participants, conversations and conversation_participants are join context and stay generic rows.
func ClaimableTables() []string { return []string{tableMessages, tableParts} }

// claimsOf are the claims of tables on the db role (restated from androidtelegram, F29).
func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// stateNotJoinedRecovered is the state of every join a recovered messages row does not make (spec 8.2, 4I3 B2).
const stateNotJoinedRecovered = "not-joined-recovered"

// The values of raw.part_join and raw.part_join_reason (spec 8.2, 8.3).
const (
	partJoined           = "joined"
	partNotJoined        = "not-joined"
	joinNoAutoincrement  = "no-autoincrement"
	joinNoSequence       = "no-sqlite-sequence"
	joinNoLiveMessage    = "no-live-message"
	joinConvDiffers      = "conversation-differs"
	joinTimestampDiffers = "timestamp-differs"
	joinUndecidedReason  = stateUndecided
)

// noteBasisUndecided is the note that states why the join basis could not be decided (once per run).
const noteBasisUndecided = "part_join_basis_undecided"

// joinBasis is spec 8.3 condition 1 for one run: Reason "" when the live schema of messages declares _id INTEGER PRIMARY KEY
// AUTOINCREMENT and sqlite_sequence holds a messages row (no _id is reused), else the reason no recovered part joins:
// no-autoincrement, no-sqlite-sequence or undecided.
type joinBasis struct{ Reason string }

// joinBasisOf decides spec 8.3 condition 1 from the live view (4I5-P7): the table row of messages in sqlite_master and the
// messages row of sqlite_sequence, each matched by name under the engine's equality. A lookup that fails or is undecidable, a
// schema row that cannot be read, or no table row of messages is undecided (the cause in the note
// part_join_basis_undecided), never a basis. The partsMapping(nil) closure calls it once per run.
func joinBasisOf(m parse.MapContext) joinBasis {
	undecided := func(cause string) joinBasis {
		m.Note(noteBasisUndecided, cause)
		return joinBasis{Reason: joinUndecidedReason}
	}
	rows, short, err := matchText(m, "sqlite_master", "name", tableMessages)
	switch {
	case err != nil:
		return undecided("the sqlite_master lookup of messages failed: " + err.Error())
	case short:
		return undecided("the sqlite_master lookup of messages is undecidable")
	}
	typeCol, sqlCol := m.Column("sqlite_master", "type"), m.Column("sqlite_master", "sql")
	found, auto := false, false
	for _, r := range rows {
		ty, k := textCellOf(r, typeCol)
		if k != rowrun.CellValue {
			return undecided("the type of a sqlite_master row of messages cannot be read")
		}
		if string(ty) != "table" {
			continue
		}
		sql, k := textCellOf(r, sqlCol)
		if k != rowrun.CellValue {
			return undecided("the sql of the sqlite_master row of messages cannot be read")
		}
		found, auto = true, autoincrementID(string(sql))
	}
	switch {
	case !found:
		return undecided("sqlite_master holds no table row of messages")
	case !auto:
		return joinBasis{Reason: joinNoAutoincrement}
	}
	rows, short, err = matchText(m, "sqlite_sequence", "name", tableMessages)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return joinBasis{Reason: joinNoSequence}
	case err != nil:
		return undecided("the sqlite_sequence lookup of messages failed: " + err.Error())
	case len(rows) > 0:
		return joinBasis{}
	case short:
		return undecided("the sqlite_sequence lookup of messages is undecidable")
	}
	return joinBasis{Reason: joinNoSequence}
}

// matchText is rowrun.Match for a TEXT key: the rows of table whose col equals key under the engine's equality, and whether
// the answer is undecided (an undecidable key, or a hit on an index that lost rows: a possibly short answer).
func matchText(m parse.MapContext, table, col, key string) ([]parse.Row, bool, error) {
	hits, _ := m.(undecidedHits)
	before := int64(0)
	if hits != nil {
		before = hits.UndecidedHits(table, col)
	}
	rows, err := m.Match(table, col, sqlitedb.LiteralTextKey([]byte(key)))
	switch {
	case errors.Is(err, sqlitedb.ErrKeyUndecidable):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return rows, hits != nil && hits.UndecidedHits(table, col) > before, nil
}

// autoincrementID says the _id column definition of a CREATE TABLE statement holds the words PRIMARY, KEY and AUTOINCREMENT
// (ASCII case-insensitive; a quoted word or one inside parentheses is not a word of the definition). A statement without an
// _id column is false.
func autoincrementID(createSQL string) bool {
	for _, def := range columnDefs(createSQL) {
		words := strings.Fields(def)
		if len(words) == 0 || !asciiEqualFold(unquoteName(words[0]), "_id") {
			continue
		}
		var primary, key, auto bool
		for _, w := range words[1:] {
			primary = primary || asciiEqualFold(w, "PRIMARY")
			key = key || asciiEqualFold(w, "KEY")
			auto = auto || asciiEqualFold(w, "AUTOINCREMENT")
		}
		return primary && key && auto
	}
	return false
}

// columnDefs splits the body of a CREATE TABLE statement at its top-level commas: a comma inside parentheses or a quoted
// string or name does not split. The text inside parentheses is blanked and the white space inside quotes is joined, so no
// word inside either is read as a word of the definition (a quoted word keeps its quotes).
func columnDefs(sql string) []string {
	open := strings.IndexByte(sql, '(')
	if open < 0 {
		return nil
	}
	var out []string
	var cur []byte
	depth := 0
	var quote byte
	for i := open + 1; i < len(sql); i++ {
		c := sql[i]
		if quote != 0 {
			switch c {
			case quote:
				quote = 0
			case ' ', '\t', '\n', '\r', '\f', '\v':
				c = '_'
			}
			if depth > 0 {
				c = ' '
			}
			cur = append(cur, c)
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '[':
			quote = ']'
		case '(':
			depth++
			c = ' '
		case ')':
			if depth == 0 {
				return append(out, string(cur))
			}
			depth--
			c = ' '
		case ',':
			if depth == 0 {
				out = append(out, string(cur))
				cur = nil
				continue
			}
		}
		if depth > 0 {
			c = ' '
		}
		cur = append(cur, c)
	}
	return append(out, string(cur))
}

// unquoteName is a column name without its SQL quotes ("x", `x`, [x], 'x').
func unquoteName(s string) string {
	if len(s) >= 2 {
		switch s[0] {
		case '"', '`', '\'':
			if s[len(s)-1] == s[0] {
				return s[1 : len(s)-1]
			}
		case '[':
			if s[len(s)-1] == ']' {
				return s[1 : len(s)-1]
			}
		}
	}
	return s
}

// partJoin checks spec 8.3 for one recovered part, in order: the basis (condition 1); ok, the part's message_id,
// conversation_id and timestamp read as integers (a cell that is not is undecided); the live messages row of msgID, by a
// decided Get (condition 2); its conversation_id equal to convID and its received_timestamp equal to ts (C5a). It returns the
// live row when every condition holds, else the first failing reason. A live cell that is not an integer is undecided; a
// join that cannot be decided is undecided; a budget, context or I/O failure is the error.
func partJoin(m parse.MapContext, b joinBasis, msgID, convID, ts int64, ok bool) (live parse.Row, reason string, err error) {
	if b.Reason != "" {
		return nil, b.Reason, nil
	}
	if !ok {
		return nil, joinUndecidedReason, nil
	}
	row, found, err := m.Get(tableMessages, msgID)
	switch {
	case err != nil && joinUndecided(err):
		return nil, joinUndecidedReason, nil
	case err != nil:
		return nil, "", err
	case !found:
		return nil, joinNoLiveMessage, nil
	}
	c := rowrun.Resolve(m, tableMessages, "conversation_id", "received_timestamp")
	lc, ck := intCellOf(row, c.At("conversation_id"))
	switch {
	case ck != rowrun.CellValue:
		return nil, joinUndecidedReason, nil
	case lc != convID:
		return nil, joinConvDiffers, nil
	}
	lt, tk := intCellOf(row, c.At("received_timestamp"))
	switch {
	case tk != rowrun.CellValue:
		return nil, joinUndecidedReason, nil
	case lt != ts:
		return nil, joinTimestampDiffers, nil
	}
	return row, "", nil
}

// mapRecoveredPart is the typed mapping of one recovered parts row (spec 8.2, B1): a record of its own columns (its Body or
// attachment, part_rowid, part_message_id, the thread by its own conversation_id) whose raw.part_join states the spec 8.3
// join. A joined part copies the channel, the direction and the participants of the live message as the live mapping
// derives them (its members read without the job cache), with raw.joined_message_rowid; nothing else of the live record (no
// guid, Time, status, subscription or read). A part that is not joined states its reason, channel and direction unknown and
// participants_unknown. basis is the run's, decided by the caller.
func mapRecoveredPart(m parse.MapContext, row parse.Row, basis *joinBasis) ([]records.Record, error) {
	c := rowrun.Resolve(m, tableParts, specNames(partsSpecs())...)
	mid, mk := intCellOf(row, c.At("message_id"))
	p, err := ownPart(m, row, c, mid, mk)
	if err != nil {
		return nil, err
	}
	conv, ck := intCellOf(row, c.At("conversation_id"))
	ts, tk := intCellOf(row, c.At("timestamp"))
	live, reason, err := partJoin(m, *basis, mid, conv, ts, mk == rowrun.CellValue && ck == rowrun.CellValue && tk == rowrun.CellValue)
	if err != nil {
		return nil, err
	}
	counterparty := ""
	if reason != "" {
		p.raw["part_join"], p.raw["part_join_reason"] = partNotJoined, reason
	} else {
		lp, err := liveParties(m, live)
		if err != nil {
			return nil, err
		}
		p.raw["part_join"], p.raw["joined_message_rowid"] = partJoined, mid
		p.msg.Channel, p.msg.Direction = lp.Channel, lp.Direction
		p.msg.Participants, p.msg.ParticipantsUnknown = lp.Participants, lp.Unknown
		counterparty = lp.Counterparty
	}
	return []records.Record{p.finish(row, counterparty)}, nil
}

// joinedParties are what a joined part copies from its live message (B1).
type joinedParties struct {
	Channel, Direction string
	Participants       []message.Participant
	Unknown            bool
	Counterparty       string
}

// liveParties derives the channel, the direction (after the cross-check, B7) and the participants of a live messages row as
// the live mapping does (spec 6.1 to 6.3), with no guard and no member cache. The raw keys and unread cells of the live row
// are its own record's, not the part's: they are not kept.
func liveParties(m parse.MapContext, row parse.Row) (joinedParties, error) {
	c := rowrun.Resolve(m, tableMessages, specNames(messagesSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	ch := channelOf(row, c.At("message_protocol"), raw, unknown)
	dir, _ := statusOf(row, c.At("message_status"), raw, unknown)
	pr, err := partiesOf(m, &env{}, row, c, raw, unknown, dir)
	if err != nil {
		return joinedParties{}, err
	}
	return joinedParties{Channel: ch, Direction: pr.Direction, Participants: pr.Participants, Unknown: pr.Unknown, Counterparty: pr.Counterparty}, nil
}
