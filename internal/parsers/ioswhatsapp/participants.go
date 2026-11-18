package ioswhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// States of a JID that is not usable as an address. The vocabulary of raw.chat_state and raw.sender_state (4I2-R7) is
// these plus the join states of message.go.
const (
	stateEmpty     = "empty"     // NULL or empty text: the callers name it no-contact-jid or no-member-jid
	stateAltered   = "altered"   // cleaning changed the stored text (NUL, invalid UTF-8, a cut): it names no address (4I2-R6)
	stateTooLong   = "too-long"  // longer than the message contract allows; never cut, a cut address is another address
	stateUndecided = "undecided" // the cell cannot be read (another storage class, damage) or its join cannot be decided
)

// jidRef is a JID read from a cell. State is "" when the JID is usable as an address: then Text is the stored text and
// JID its structure (nil when it does not parse). In the state altered Text is the cleaned text, for raw only: an altered
// JID is never parsed, never a thread id, a group or a participant.
type jidRef struct {
	Text  string
	JID   *wajid.JID
	State string
}

func (j jidRef) usable() bool { return j.State == "" }

// class is the wajid class of a usable JID, "unparseable" when its text does not parse.
func (j jidRef) class() string {
	if j.JID == nil {
		return "unparseable"
	}
	return string(j.JID.Class)
}

// jidUnknownKey is the unknown_columns key of the column a JID field is read from.
func jidUnknownKey(field string) string {
	switch field {
	case "chat_jid":
		return prefixSession + "ZCONTACTJID"
	case "member_jid_text":
		return prefixMember + "ZMEMBERJID"
	}
	return field
}

// readJID reads the JID in column col of row. CleanText comes first: a JID that cleaning changed is altered and its
// cleaned text goes to raw[field] with text_flags (4I2-R6, the 4I carry-over); a cell that cannot be read is stated in
// unknown and is undecided, never empty.
func readJID(row parse.Row, col int, raw, unknown map[string]any, field string) jidRef {
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellNull:
		return jidRef{State: stateEmpty}
	case rowrun.CellValue:
	default:
		rowrun.Flag(unknown, row, jidUnknownKey(field), col, k)
		return jidRef{State: stateUndecided}
	}
	if len(b) == 0 {
		return jidRef{State: stateEmpty}
	}
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
		raw[field] = cl.Text
		return jidRef{Text: cl.Text, State: stateAltered}
	}
	if len(cl.Text) > maxAddr {
		return jidRef{State: stateTooLong}
	}
	j := jidRef{Text: cl.Text}
	if p, err := wajid.Parse(cl.Text); err == nil {
		j.JID = &p
	}
	return j
}

// participantsOf decides who a message is from or to (spec 6.4.3, R3). The device owner's JID is not in the database, so
// an outgoing message has a "to" and no "from". An incoming status update has no attributed author. A group sender is
// the member row's JID, never the chat (the group) or ZFROMJID, which holds the group. group and status are the
// caller's decision (session type or the JID's class); sender is the member of an incoming group row (its State says
// why there is none). An incoming 1:1 message is from the chat when the chat JID names a person (user, lid or
// newsletter); every other incoming message, and every message of an unknown direction, has unknown participants.
func participantsOf(dir string, chat jidRef, group, status bool, sender jidRef) (ps []message.Participant, unknown bool) {
	switch {
	case dir == message.DirOut && chat.usable():
		return []message.Participant{{Role: message.RoleTo, Address: chat.Text}}, false
	case dir == message.DirIn && status:
		return nil, true
	case dir == message.DirIn && group:
		if sender.usable() {
			return []message.Participant{{Role: message.RoleFrom, Address: sender.Text}}, false
		}
		return nil, true
	case dir == message.DirIn && chat.usable():
		switch chat.class() {
		case string(wajid.ClassUser), string(wajid.ClassLID), string(wajid.ClassNewsletter):
			return []message.Participant{{Role: message.RoleFrom, Address: chat.Text}}, false
		}
	}
	return nil, true
}
