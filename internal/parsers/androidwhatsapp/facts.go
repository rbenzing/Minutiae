package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// code is a stored enum cell: the column it came from, its value when it is a usable integer, and the stored value
// as it must appear in raw (an unusable cell keeps its stored value there and never gets a label).
type code struct {
	Column string
	Value  int64
	OK     bool
	Raw    any
}

// readCode reads an enum column into raw as stored. A cell that is not an INTEGER is stated in unknown_columns. A
// legacy media_wa_type (TEXT affinity) is read strictly: an INTEGER cell, or TEXT that is a decimal with no sign, no
// leading zero and no other character; anything else is not usable and keeps its stored value.
func readCode(r cellReader, name string, textTyped bool) code {
	c := code{Column: name}
	if textTyped {
		col := r.cols.At(name)
		if v, k := rowrun.IntCell(r.row, col); k == rowrun.CellValue {
			c.Value, c.OK = v, true
		} else if b, k := rowrun.TextCell(r.row, col); k == rowrun.CellValue {
			c.Value, c.OK = strictDecimal(b)
		} else if k == rowrun.CellUnknown {
			rowrun.Flag(r.unknown, r.row, name, col, k)
		}
	} else {
		c.Value, c.OK = r.int(name)
	}
	if c.Raw = r.stored(name); c.Raw != nil {
		r.raw[name] = c.Raw
	}
	return c
}

// strictDecimal parses ^(0|-?[1-9][0-9]{0,17})$.
func strictDecimal(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 19 {
		return 0, false
	}
	if len(b) == 1 && b[0] == '0' {
		return 0, true
	}
	i, neg := 0, false
	if b[0] == '-' {
		neg, i = true, 1
	}
	if i >= len(b) || b[i] < '1' || b[i] > '9' || len(b)-i > 18 {
		return 0, false
	}
	var n int64
	for ; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			return 0, false
		}
		n = n*10 + int64(b[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

// label is the research label of a usable code, and whether the research supports the value.
func (c code) label() (string, bool) {
	if !c.OK {
		return "", false
	}
	return lookupEnum(c.Column, c.Value)
}

// addr is an address resolved from a jid row or a stored JID string. Str is the JID as stored (cleaned, never
// composed): empty when the address is absent. Parts holds the user, server, agent and device of a jid row whose
// raw_string is absent.
type addr struct {
	Str   string
	JID   *wajid.JID
	Parts map[string]any
	State string // "": resolved; else why the address is absent
	Show  string // the cleaned text of an altered address (stateAltered): raw only, never an address
	RowID *int64 // the jid row the address was read from, nil for an address read from stored text
}

// States of an address that is absent.
const (
	stateAbsent      = "absent"        // the column says there is none (0, -1, NULL, empty)
	stateNoJIDRow    = "no-jid-row"    // the key names no jid row
	stateNoRawString = "no-raw-string" // the jid row has no raw_string
	stateTooLong     = "too-long"      // the address is longer than the contract allows
	stateUndecided   = "undecided"     // the join to the jid row cannot be decided
	stateAltered     = "altered"       // cleaning changed the stored text (NUL, invalid UTF-8): it names no address that can be shown to exist (4I fix 5)
)

// addrFromText builds an address from stored text. An over-long JID is never cut: a cut address is another address. A JID
// that cleaning changed is unknown (stateAltered), with the cleaned text in Show.
func addrFromText(raw map[string]any, field string, b []byte) addr {
	if len(b) == 0 {
		return addr{State: stateAbsent}
	}
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	if len(cl.Text) > maxAddr {
		return addr{State: stateTooLong}
	}
	if len(cl.Flags) > 0 {
		// a repaired JID is not the stored JID: it is never parsed, never a thread id or a grouping key (identity from
		// structure, never from a guess); the cleaned text stays in raw with its text_flags
		return addr{State: stateAltered, Show: cl.Text}
	}
	a := addr{Str: cl.Text}
	if j, err := wajid.Parse(cl.Text); err == nil {
		a.JID = &j
	}
	return a
}

// addrFromJID reads the address of a jid row: its raw_string, or when that is absent the parts as stored.
func addrFromJID(m parse.MapContext, row parse.Row, raw, unknown map[string]any, field string) addr {
	c := rowrun.Resolve(m, "jid", "raw_string", "user", "server", "agent", "device")
	var rowid *int64
	if id, ok := row.Rowid(); ok {
		rowid = &id
	}
	if b, k := rowrun.TextCell(row, c.At("raw_string")); k == rowrun.CellValue && len(b) > 0 {
		a := addrFromText(raw, field, b)
		a.RowID = rowid
		return a
	}
	r := cellReader{row: row, cols: c, raw: raw, unknown: unknown, prefix: "jid."}
	a := addr{State: stateNoRawString, Parts: map[string]any{}, RowID: rowid}
	for _, n := range []string{"user", "server"} {
		if b, ok := r.text(n); ok {
			a.Parts[n] = cleanField(raw, field+"_"+n, b)
		}
	}
	for _, n := range []string{"agent", "device"} {
		if v, ok := r.int(n); ok {
			a.Parts[n] = v
		}
	}
	return a
}

// group says whether the address is a group: from the classified server of a JID string, or the server of the parts.
func (a addr) group() *bool {
	var g bool
	switch {
	case a.JID != nil:
		g = a.JID.Class == wajid.ClassGroup
	case a.Parts != nil:
		s, ok := a.Parts["server"].(string)
		if !ok {
			return nil
		}
		g = s == "g.us"
	default:
		return nil
	}
	return &g
}

func (a addr) isStatus() bool { return a.JID != nil && a.JID.Class == wajid.ClassStatus }

// msgFacts is a message row as the one builder needs it, whatever the generation it came from (spec 6.2).
type msgFacts struct {
	Table string // messages | message: raw.source_table
	Place rowrun.Place
	Rowid int64

	Dir, Type, Status code
	LegacySys         bool // legacy type 0 with status 6: a system row, not text (spec 7.1)

	Chat      addr
	ChatState string // why the chat is unknown (no chat row, no jid row), "" when it is known or only its string is absent
	Title     string

	Sender addr // the author column: modern sender_jid_row_id, legacy remote_resource

	KeyID      string
	Text       []byte
	TextSource string
	Time       *rowrun.Instant
	Received   *rowrun.Instant
	Delivered  *rowrun.Instant

	Revoke   *revokeFacts   // a "deleted for everyone" tombstone (spec 6.5), nil for a live message
	Reaction *reactionFacts // a message_add_on reaction (4I-R20), nil for a message

	Media *mediaFacts // the file of a media message (spec 6.7), nil for none
	Sole  func() bool // R5a, answered once per parse: the database is the only live one of its app

	Purge    purgeResult // the deleted_chat_job outcome of the row (4I-R4); for a reaction, the parent's
	PurgeVia string      // "parent" for a reaction that follows its parent's outcome

	Raw, Unknown map[string]any
}
