package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// Reactions (spec 6.5 as amended by 4I-R20): each message_add_on row of type 56 is a record of its own. Its direction
// and its sender rest on one source each (message_add_on.from_me, sender_jid_row_id: research 4.5), so the direction is
// unknown and there is no participant; the stored values stay in raw. Rows of another type are neither records nor
// established, and give no error.

// reactionFacts is what makes a msgFacts a reaction: the key of the message it reacts to.
type reactionFacts struct{ Target string }

// addOnTypeReaction is the add-on type that is a reaction (research 4.5, 4I-R7).
const addOnTypeReaction = 56

func addOnSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "message_add_on_type", Required: true, Affinity: "integer"},
		{Name: "parent_message_row_id", Required: true, Affinity: "integer"},
		{Name: "chat_row_id", Affinity: "integer"},
		{Name: "from_me", Affinity: "integer"},
		{Name: "key_id", Affinity: "text"},
		{Name: "sender_jid_row_id", Affinity: "integer"},
		{Name: "timestamp", Affinity: "integer"},
	}
}

func addOnMapping() parse.TableMapping {
	return parse.TableMapping{Table: tblAddOn, Columns: addOnSpecs(), Map: mapAddOn}
}

func mapAddOn(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the message_add_on row has no rowid, so its joins cannot be made")
	}
	f := msgFacts{Table: tblAddOn, Place: place, Rowid: id, Raw: map[string]any{}, Unknown: map[string]any{}}
	r := cellReader{row: row, cols: rowrun.Resolve(m, tblAddOn, specNames(addOnSpecs())...), raw: f.Raw, unknown: f.Unknown}
	typ, ok := r.int("message_add_on_type")
	if !ok {
		return nil, rowrun.Unmappable("message_add_on_type is not an INTEGER, so the kind of the add-on is unknown")
	}
	if typ != addOnTypeReaction {
		return nil, nil
	}
	f.Reaction = &reactionFacts{}
	if fl := rowrun.DamageFlags(row); fl != nil {
		f.Raw["row_flags"] = fl
	}
	f.Raw["message_add_on_type"] = typ
	if l, ok := lookupEnum(colAddOnType, typ); ok {
		f.Raw["label_message_add_on_type"] = l
	}
	if v := r.stored("from_me"); v != nil {
		f.Raw["from_me"] = v
	}
	if b, ok := r.text("key_id"); ok {
		f.KeyID = cleanField(f.Raw, "guid", b)
	}
	f.Time = readInstant(r, "timestamp")
	if v, ok := r.int("chat_row_id"); ok {
		f.Raw["add_on_chat_row_id"] = v
	}
	if err := readModernSender(m, r, &f); err != nil {
		return nil, err
	}
	f.Sender = addr{State: stateAbsent} // a reaction's sender is never a participant (4I-R20)
	if err := readReactionParent(m, r, &f); err != nil {
		return nil, err
	}
	if err := readReactionText(m, &f); err != nil {
		return nil, err
	}
	return build(m, f)
}

// readReactionParent joins the message the reaction is on: its key_id is the target and its chat is the thread. With no
// parent the thread is the add-on's own chat, and the state says why there is no target.
func readReactionParent(m parse.MapContext, r cellReader, f *msgFacts) error {
	pid, ok := r.int("parent_message_row_id")
	if !ok {
		f.Raw["parent_state"] = "no-parent-key"
		return readModernChat(m, r, f)
	}
	prow, st, err := getRow(m, tblMessage, pid)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.Raw["parent_state"] = "no-message-row"
		return readModernChat(m, r, f)
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, tblMessage)
		return readModernChat(m, r, f)
	}
	pr := cellReader{row: prow, cols: rowrun.Resolve(m, tblMessage, "chat_row_id", "key_id"), raw: f.Raw, unknown: f.Unknown, prefix: "message."}
	if b, ok := pr.text("key_id"); ok && len(b) > 0 {
		f.Reaction.Target = cleanField(f.Raw, "target_guid", b)
	}
	return readModernChat(m, pr, f)
}

// readReactionText reads message_add_on_reaction (keyed by message_add_on_row_id, its rowid alias): the reaction is the
// body. An absent table, a missing row and an unreadable cell are stated; an empty or NULL text is an empty reaction
// (no body, raw.reaction_empty, its meaning not established).
func readReactionText(m parse.MapContext, f *msgFacts) error {
	const tbl = "message_add_on_reaction"
	if badTable(m, tbl) {
		undecidedJoin(f.Raw, tbl)
		return nil
	}
	if m.Column(tbl, "message_add_on_row_id") < 0 {
		f.Raw["reaction_state"] = "no-reaction-table"
		return nil
	}
	row, st, err := getRow(m, tbl, f.Rowid)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.Raw["reaction_state"] = "no-reaction-row"
		return nil
	case jNoTable:
		f.Raw["reaction_state"] = "no-reaction-table"
		return nil
	case jUndecided:
		undecidedJoin(f.Raw, tbl)
		return nil
	}
	r := sideReader(m, f, row, tbl, "reaction", "sender_timestamp")
	if v, ok := r.int("sender_timestamp"); ok {
		f.Raw["sender_timestamp"], f.Raw["sender_timestamp_unit"] = v, unitUnestablished
	}
	col := r.cols.At("reaction")
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellValue:
		if len(b) == 0 {
			f.Raw["reaction_empty"] = true
		} else {
			f.Text, f.TextSource = b, textSourceText
		}
	case rowrun.CellNull:
		f.Raw["reaction_empty"] = true
	case rowrun.CellMissing:
		f.Unknown[tbl+".reaction"] = "missing"
		f.Raw["reaction_state"] = "unreadable"
	default:
		rowrun.Flag(f.Unknown, row, tbl+".reaction", col, k)
		f.Raw["reaction_state"] = "unreadable"
	}
	return nil
}
