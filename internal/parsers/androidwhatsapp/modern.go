package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// modernRawInts are the INTEGER columns of message kept in raw as stored, without an interpretation (spec 6.4.2).
func modernRawInts() []string {
	return []string{"sort_id", "starred", "origination_flags", "broadcast", "recipient_count", "message_add_on_flags", "view_mode", "lookup_tables"}
}

// modernSpecs is the message mapping's column list; the Required columns are the ones Probe insists on.
func modernSpecs() []parse.ColumnSpec {
	specs := []parse.ColumnSpec{
		{Name: "chat_row_id", Required: true, Affinity: "integer"},
		{Name: "from_me", Required: true, Affinity: "integer"},
		{Name: "key_id", Required: true, Affinity: "text"},
		{Name: "timestamp", Required: true, Affinity: "integer"},
		{Name: "message_type", Required: true, Affinity: "integer"},
		{Name: "sender_jid_row_id", Affinity: "integer"},
		{Name: "status", Affinity: "integer"},
		{Name: "received_timestamp", Affinity: "integer"},
		{Name: "receipt_server_timestamp", Affinity: "integer"},
		{Name: "text_data", Affinity: "text"},
	}
	for _, n := range modernRawInts() {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "integer"})
	}
	return specs
}

func messageMapping() parse.TableMapping {
	return parse.TableMapping{Table: tblMessage, Columns: modernSpecs(), Map: mapModern}
}

func mapModern(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	f, err := readModern(m, row)
	if err != nil {
		return nil, err
	}
	return build(m, f)
}

// readModern reads a message row and its joins into the generation-neutral facts (spec 6.4.1).
func readModern(m parse.MapContext, row parse.Row) (msgFacts, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return msgFacts{}, err
	}
	id, ok := row.Rowid()
	if !ok {
		return msgFacts{}, rowrun.Unmappable("the message row has no rowid, so its joins cannot be made")
	}
	f := msgFacts{Table: tblMessage, Place: place, Rowid: id, Raw: map[string]any{}, Unknown: map[string]any{}}
	r := cellReader{row: row, cols: rowrun.Resolve(m, tblMessage, specNames(modernSpecs())...), raw: f.Raw, unknown: f.Unknown}
	if fl := rowrun.DamageFlags(row); fl != nil {
		f.Raw["row_flags"] = fl
	}

	f.Dir = readCode(r, colFromMe, false)
	f.Type = readCode(r, colMessageType, false)
	f.Status = readCode(r, "status", false)
	if b, ok := r.text("key_id"); ok {
		f.KeyID = cleanField(f.Raw, "guid", b)
	}
	f.Time = readInstant(r, "timestamp")
	f.Received = readInstant(r, "received_timestamp")
	if v, ok := r.int("receipt_server_timestamp"); ok {
		f.Raw["receipt_server_timestamp"] = v
		f.Raw["receipt_server_timestamp_unit"] = unitUnestablished
	}
	r.rawInts(modernRawInts()...)

	if err := readModernChat(m, r, &f); err != nil {
		return msgFacts{}, err
	}
	if err := readModernSender(m, r, &f); err != nil {
		return msgFacts{}, err
	}
	if b, ok := r.text("text_data"); ok {
		f.Text, f.TextSource = b, textSourceText
	}
	if f.Type.OK && f.Type.Value == typeContactCard {
		if err := readVCards(m, &f); err != nil {
			return msgFacts{}, err
		}
	}
	return f, nil
}

// unitUnestablished marks a time column whose unit no source establishes: kept raw, never a time (R8, spec 7.6).
const unitUnestablished = "unestablished"

// readInstant reads a message time column in milliseconds. The floor of 1973 applies: a smaller value is invalid, never
// rescaled (4I-R10). The stored value is in raw, with its status when it is not a time.
func readInstant(r cellReader, name string) *rowrun.Instant {
	v, ok := r.int(name)
	if !ok {
		return nil
	}
	in := rowrun.MillisFrom1973(v)
	in.Record(r.raw, name)
	return &in
}

// readModernChat joins the chat of the message and its JID. A dangling key is stated as raw.chat_state; a join that
// cannot be decided is listed in raw.join_undecided; neither is read as a chat with no JID.
func readModernChat(m parse.MapContext, r cellReader, f *msgFacts) error {
	id, ok := r.int("chat_row_id")
	if !ok {
		return nil
	}
	f.Raw["chat_row_id"] = id
	crow, st, err := getRow(m, "chat", id)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.ChatState = "no-chat-row"
		return nil
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "chat")
		return nil
	}
	cr := cellReader{row: crow, cols: rowrun.Resolve(m, "chat", "jid_row_id", "subject"), raw: f.Raw, unknown: f.Unknown, prefix: "chat."}
	if b, ok := cr.text("subject"); ok && len(b) > 0 {
		f.Title = cleanField(f.Raw, "thread.title", b)
	}
	jid, ok := cr.int("jid_row_id")
	if !ok {
		f.ChatState = stateNoJIDRow
		return nil
	}
	jrow, st, err := getRow(m, "jid", jid)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.ChatState = stateNoJIDRow
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "jid")
	default:
		f.Chat = addrFromJID(m, jrow, f.Raw, f.Unknown, "chat_jid")
		if f.Chat.Parts != nil {
			f.Raw["chat_jid_parts"] = f.Chat.Parts
		}
	}
	return nil
}

// readModernSender joins the author of the message: sender_jid_row_id above 0 names a jid row; 0, -1 and NULL name none
// (research 4.1). The stored value is in raw.
func readModernSender(m parse.MapContext, r cellReader, f *msgFacts) error {
	sid, ok := r.int("sender_jid_row_id")
	if ok {
		f.Raw["sender_jid_row_id"] = sid
	}
	if !ok || sid <= 0 {
		f.Sender = addr{State: stateAbsent}
		return nil
	}
	jrow, st, err := getRow(m, "jid", sid)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.Sender = addr{State: stateNoJIDRow}
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "jid")
		f.Sender = addr{State: stateUndecided}
	default:
		f.Sender = addrFromJID(m, jrow, f.Raw, f.Unknown, "sender_jid")
		if f.Sender.Parts != nil {
			f.Raw["sender_jid_parts"] = f.Sender.Parts
		}
		if f.Sender.Str != "" {
			f.Raw["sender_jid"] = f.Sender.Str
		}
	}
	return nil
}

// readVCards makes the body of a contact card from its message_vcard rows (spec 6.4.2): the cards in rowid order
// joined with a newline, at most maxList of them. With no card row the body stays text_data and the state is stated.
func readVCards(m parse.MapContext, f *msgFacts) error {
	rows, st, err := matchInt(m, "message_vcard", "message_row_id", f.Rowid)
	if err != nil {
		return err
	}
	switch st {
	case jNoTable:
		return nil
	case jUndecided:
		undecidedJoin(f.Raw, "message_vcard")
		return nil
	}
	vc := rowrun.Resolve(m, "message_vcard", "vcard")
	var cards [][]byte
	var total int
	for _, row := range rows {
		b, k := rowrun.TextCell(row, vc.At("vcard"))
		if k != rowrun.CellValue {
			continue
		}
		total++
		if len(cards) < maxList {
			cards = append(cards, b)
		}
	}
	if total == 0 {
		f.Raw["vcard_state"] = "no-message_vcard-row"
		return nil
	}
	if total > maxList {
		f.Raw["vcards_total"], f.Raw["vcards_truncated"] = int64(total), true
	}
	f.Text, _ = joinText(cards)
	f.TextSource = textSourcePart
	return nil
}
