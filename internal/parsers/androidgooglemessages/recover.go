package androidgooglemessages

import (
	"errors"
	"slices"
	"strconv"
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

// joinBasis is what the typed pass decides once per run (4I5-P7). Reason is spec 8.3 condition 1: "" when the live schema of
// messages declares _id INTEGER PRIMARY KEY AUTOINCREMENT and sqlite_sequence holds a messages row (no _id is reused), else
// the reason no recovered part joins: no-autoincrement, no-sqlite-sequence or undecided. Guard names the join tables whose
// _id is not shown to be the rowid alias (I2): a party join through one of them is undecided, never attached by rowid.
type joinBasis struct {
	Reason string
	Guard  *joinGuard
}

// guardedTables are the join tables a joined part reaches by rowid: each one's _id must be the rowid alias.
var guardedTables = []string{tableParticipants, tableConversations}

// joinBasisOf decides the run's join basis from the live view (4I5-P7, I2, I3, M1, M3): the one table row of messages in
// sqlite_master (autoincrementID on its sql) and the messages row of sqlite_sequence, each matched by name under the
// engine's equality, and the table rows of the join tables reached by rowid (rowidAliasID). Data damage, an undecidable
// lookup, a schema row that cannot be read, no table row or two of them for messages, or a sqlite_sequence the schema read
// does not show is undecided (the cause in the note part_join_basis_undecided), never a basis; a join table that cannot be
// decided is guarded (the note join_undecided.<table>). A budget, context or I/O failure is the error. The partsMapping(nil)
// closure calls it once per run.
func joinBasisOf(m parse.MapContext) (joinBasis, error) {
	b := joinBasis{Guard: &joinGuard{}}
	for _, t := range guardedTables {
		sql, cause, found, err := tableSQL(m, t)
		switch {
		case err != nil:
			return joinBasis{}, err
		case cause != "":
			b.Guard.add(t, "undecided: "+cause)
		case found && !rowidAliasID(sql):
			b.Guard.add(t, "column _id is not the rowid alias")
		}
	}
	for _, t := range b.Guard.order {
		reason, _ := b.Guard.reason(t)
		m.Note("join_undecided."+t, reason)
	}
	undecided := func(cause string) (joinBasis, error) {
		m.Note(noteBasisUndecided, cause)
		b.Reason = joinUndecidedReason
		return b, nil
	}
	sql, cause, found, err := tableSQL(m, tableMessages)
	switch {
	case err != nil:
		return joinBasis{}, err
	case cause != "":
		return undecided(cause)
	case !found:
		return undecided("sqlite_master holds no table row of messages")
	case !autoincrementID(sql):
		b.Reason = joinNoAutoincrement
		return b, nil
	}
	rows, short, err := matchText(m, "sqlite_sequence", "name", tableMessages)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return undecided("sqlite_sequence is not in the schema read")
	case err != nil && dataDamage(err):
		return undecided("the sqlite_sequence lookup of messages failed: " + err.Error())
	case err != nil:
		return joinBasis{}, err
	case len(rows) > 0:
		return b, nil
	case short:
		return undecided("the sqlite_sequence lookup of messages is undecidable")
	}
	b.Reason = joinNoSequence
	return b, nil
}

// tableSQL is the sql of the one table row of name in sqlite_master: found false with no cause when the schema read holds
// none; a cause when it cannot be decided (data damage, an undecidable lookup, an unreadable cell, two table rows); a budget,
// context or I/O failure is the error.
func tableSQL(m parse.MapContext, name string) (sql, cause string, found bool, err error) {
	rows, short, err := matchText(m, "sqlite_master", "name", name)
	switch {
	case err != nil && dataDamage(err):
		return "", "the sqlite_master lookup of " + name + " failed: " + err.Error(), false, nil
	case err != nil:
		return "", "", false, err
	case short:
		return "", "the sqlite_master lookup of " + name + " is undecidable", false, nil
	}
	typeCol, sqlCol := m.Column("sqlite_master", "type"), m.Column("sqlite_master", "sql")
	for _, r := range rows {
		ty, k := textCellOf(r, typeCol)
		if k != rowrun.CellValue {
			return "", "the type of a sqlite_master row of " + name + " cannot be read", false, nil
		}
		if string(ty) != "table" {
			continue
		}
		if found {
			return "", "sqlite_master holds two table rows of " + name, false, nil
		}
		s, k := textCellOf(r, sqlCol)
		if k != rowrun.CellValue {
			return "", "the sql of the sqlite_master row of " + name + " cannot be read", false, nil
		}
		sql, found = string(s), true
	}
	return sql, "", found, nil
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

// idDef is the words of the _id column definition of a CREATE TABLE statement and the words after its closing parenthesis
// (ok false when the statement has no _id column).
func idDef(createSQL string) (words, tail []string, ok bool) {
	defs, after := columnDefs(createSQL)
	for _, def := range defs {
		w := strings.Fields(def)
		if len(w) > 0 && asciiEqualFold(unquoteName(w[0]), "_id") {
			return w, strings.Fields(after), true
		}
	}
	return nil, nil, false
}

// hasWord says one of words equals w (ASCII case-insensitive).
func hasWord(words []string, w string) bool {
	return slices.ContainsFunc(words, func(x string) bool { return asciiEqualFold(x, w) })
}

// autoincrementID says the _id column definition of a CREATE TABLE statement holds the words PRIMARY, KEY and AUTOINCREMENT
// (ASCII case-insensitive; a quoted word, a word inside parentheses or inside a comment is not a word of the definition). A
// statement without an _id column is false.
func autoincrementID(createSQL string) bool {
	w, _, ok := idDef(createSQL)
	return ok && hasWord(w[1:], "PRIMARY") && hasWord(w[1:], "KEY") && hasWord(w[1:], "AUTOINCREMENT")
}

// rowidAliasID says the _id column of a CREATE TABLE statement is the rowid alias (I2): declared with the type INTEGER
// exactly and PRIMARY KEY in its own definition, not DESC, in a table that is not WITHOUT ROWID. A table constraint PRIMARY
// KEY (_id) is not read: such a table is guarded (undecided), never trusted.
func rowidAliasID(createSQL string) bool {
	w, tail, ok := idDef(createSQL)
	return ok && len(w) > 1 && asciiEqualFold(w[1], "INTEGER") && hasWord(w[1:], "PRIMARY") && hasWord(w[1:], "KEY") &&
		!hasWord(w[1:], "DESC") && !hasWord(tail, "WITHOUT")
}

// columnDefs splits the body of a CREATE TABLE statement at its top-level commas and returns the text after the body's
// closing parenthesis: a comma inside parentheses, a quoted string or name, or a comment (-- to the end of the line, /* */)
// does not split. The text inside parentheses and comments is blanked and the white space inside quotes is joined, so no
// word inside any of them is read as a word of a definition (a quoted word keeps its quotes).
func columnDefs(sql string) (defs []string, tail string) {
	var cur []byte
	depth := -1 // -1 before the body's opening parenthesis
	var quote byte
	for i := 0; i < len(sql); i++ {
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
			if depth >= 0 {
				cur = append(cur, c)
			}
			continue
		}
		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			c = ' '
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += 2 + end + 1
			}
			c = ' '
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '[':
			quote = ']'
		case c == '(':
			depth++
			if depth == 0 {
				continue
			}
			c = ' '
		case c == ')':
			if depth == 0 {
				return append(defs, string(cur)), sql[i+1:]
			}
			if depth > 0 {
				depth--
			}
			c = ' '
		case c == ',' && depth == 0:
			defs = append(defs, string(cur))
			cur = nil
			continue
		}
		if depth < 0 {
			continue
		}
		if depth > 0 {
			c = ' '
		}
		cur = append(cur, c)
	}
	if depth < 0 {
		return nil, ""
	}
	return append(defs, string(cur)), ""
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

// mapRecoveredPart is the typed mapping of one recovered parts row (spec 8.2, B1, B1a): a record of its own columns (its Body
// or attachment, part_rowid, part_message_id, the thread by its own conversation_id) whose raw.part_join states the spec 8.3
// join. A joined part copies the channel, the direction and the participants of the live message as the live mapping
// derives them (its members read without the job cache, its party joins through the run's guard), with the raw keys and
// unread cells that qualify those parties (partyRaw) and raw.joined_message_rowid; nothing of the live record's thread,
// status, subscription, read, times or guid. A members cut is stated in the note members_cut.<conversation> (the typed pass
// has no counted warning of its own: the mapping cannot warn). A part that is not joined states its reason, channel and
// direction unknown and participants_unknown. basis is the run's, decided by the caller.
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
		lp, err := liveParties(m, basis.Guard, live)
		if err != nil {
			return nil, err
		}
		p.raw["part_join"], p.raw["joined_message_rowid"] = partJoined, mid
		p.msg.Channel, p.msg.Direction = lp.Channel, lp.Direction
		p.msg.Participants, p.msg.ParticipantsUnknown = lp.Participants, !lp.ParticipantsKnown
		counterparty = lp.Counterparty
		for k, v := range lp.Raw {
			p.raw[k] = v
		}
		for _, f := range lp.TextFlags {
			markText(p.raw, f)
		}
		for k, v := range lp.Unknown {
			p.unknown[k] = v
		}
		if lp.Cut {
			total, _ := lp.Raw["members_total"].(int64)
			m.Note("members_cut."+strconv.FormatInt(lp.Conversation, 10), reasonMembersCut+": conversation "+
				strconv.FormatInt(lp.Conversation, 10)+": "+strconv.FormatInt(total, 10)+" members, "+strconv.Itoa(maxList)+" kept")
		}
	}
	return []records.Record{p.finish(row, counterparty)}, nil
}

// joinedParties are what a joined part copies from its live message (B1, B1a).
type joinedParties struct {
	Channel, Direction string
	Participants       []message.Participant
	ParticipantsKnown  bool // the live mapping states parties (not participants_unknown)
	Counterparty       string
	Raw                map[string]any // partyRaw of the live row's raw
	TextFlags          []string       // the live row's text_flags of the copied keys
	Unknown            map[string]any // the live row's unread cells of participants.* and conversation_participants.*
	Cut                bool           // the members were cut at maxList (raw.members_cut)
	Conversation       int64          // the live row's conversation_id, when Cut
}

// isPartyKey says a raw key (or a text_flags field) of the live row qualifies the parties a joined part copies (B1a):
// sender_is_self, sender_state, direction_conflict, self_state, join_undecided, blocked_member_ids and every members_* and
// from_* key.
func isPartyKey(k string) bool {
	switch k {
	case "sender_is_self", "sender_state", "direction_conflict", "self_state", "join_undecided", "blocked_member_ids":
		return true
	}
	return strings.HasPrefix(k, "members_") || strings.HasPrefix(k, "from_")
}

// liveParties derives the channel, the direction (after the cross-check, B7) and the participants of a live messages row as
// the live mapping does (spec 6.1 to 6.3), its party joins through g and with no member cache, and the raw keys and unread
// cells that qualify them (B1a). The rest of the live row's raw (its thread, self row, codes, times) is its own record's and
// is not kept.
func liveParties(m parse.MapContext, g *joinGuard, row parse.Row) (joinedParties, error) {
	c := rowrun.Resolve(m, tableMessages, specNames(messagesSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	ch := channelOf(row, c.At("message_protocol"), raw, unknown)
	dir, _ := statusOf(row, c.At("message_status"), raw, unknown)
	pr, err := partiesOf(m, &env{Guard: g}, row, c, raw, unknown, dir)
	if err != nil {
		return joinedParties{}, err
	}
	jp := joinedParties{
		Channel: ch, Direction: pr.Direction, Participants: pr.Participants, ParticipantsKnown: !pr.Unknown,
		Counterparty: pr.Counterparty, Raw: map[string]any{}, Unknown: map[string]any{},
	}
	for k, v := range raw {
		if isPartyKey(k) {
			jp.Raw[k] = v
		}
	}
	flags, _ := raw["text_flags"].([]any)
	for _, f := range flags {
		if s, ok := f.(string); ok && isPartyKey(s) {
			jp.TextFlags = append(jp.TextFlags, s)
		}
	}
	for k, v := range unknown {
		if strings.HasPrefix(k, tableParticipants+".") || strings.HasPrefix(k, tableMembers+".") {
			jp.Unknown[k] = v
		}
	}
	if cut, _ := raw["members_cut"].(bool); cut {
		jp.Cut = true
		jp.Conversation, _ = raw["conversation_id"].(int64)
	}
	return jp, nil
}
