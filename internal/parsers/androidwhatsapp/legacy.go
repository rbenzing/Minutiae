package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// legacyRawInts are the INTEGER columns of messages kept in raw as stored, without an interpretation (spec 6.4.2).
func legacyRawInts() []string {
	return []string{"starred", "needs_push", "recipient_count", "forwarded", "lookup_tables", "edit_version", "send_timestamp", "read_device_timestamp", "played_device_timestamp"}
}

// legacySpecs is the messages mapping's column list; the Required columns are the ones Probe insists on.
func legacySpecs() []parse.ColumnSpec {
	specs := []parse.ColumnSpec{
		{Name: "key_remote_jid", Required: true, Affinity: "text"},
		{Name: "key_from_me", Required: true, Affinity: "integer"},
		{Name: "key_id", Required: true, Affinity: "text"},
		{Name: "timestamp", Required: true, Affinity: "integer"},
		{Name: "media_wa_type", Required: true, Affinity: "text"},
		{Name: "status", Required: true, Affinity: "integer"},
		{Name: "data", Affinity: "text"},
		{Name: "remote_resource", Affinity: "text"},
		{Name: "received_timestamp", Affinity: "integer"},
		{Name: "receipt_device_timestamp", Affinity: "integer"},
		{Name: "receipt_server_timestamp", Affinity: "integer"},
		{Name: "media_caption", Affinity: "text"},
		{Name: "media_size", Affinity: "integer"},
		{Name: "latitude", Affinity: "real"},
		{Name: "longitude", Affinity: "real"},
		{Name: "quoted_row_id", Affinity: "integer"},
	}
	for _, n := range legacyRawInts() {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "integer"})
	}
	return specs
}

func messagesMapping() parse.TableMapping {
	return parse.TableMapping{Table: tblMessages, Columns: legacySpecs(), Map: mapLegacy}
}

func mapLegacy(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	f, err := readLegacy(m, row)
	if err != nil {
		return nil, err
	}
	return build(m, f)
}

// readLegacy reads a messages row and its joins into the generation-neutral facts (spec 6.4.1). The legacy sentinel
// (TEXT key_remote_jid '-1') is not a message.
func readLegacy(m parse.MapContext, row parse.Row) (msgFacts, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return msgFacts{}, err
	}
	c := rowrun.Resolve(m, tblMessages, specNames(legacySpecs())...)
	if kb, k := rowrun.TextCell(row, c.At("key_remote_jid")); k == rowrun.CellValue && string(kb) == sentinelJID {
		return msgFacts{}, rowrun.Unmappable("whatsapp legacy sentinel row (key_remote_jid = '-1'): not a message")
	}
	id, ok := row.Rowid()
	if !ok {
		return msgFacts{}, rowrun.Unmappable("the messages row has no rowid, so its joins cannot be made")
	}
	f := msgFacts{Table: tblMessages, Place: place, Rowid: id, Raw: map[string]any{}, Unknown: map[string]any{}}
	r := cellReader{row: row, cols: c, raw: f.Raw, unknown: f.Unknown}
	if fl := rowrun.DamageFlags(row); fl != nil {
		f.Raw["row_flags"] = fl
	}

	f.Dir = readCode(r, colKeyFromMe, false)
	f.Type = readCode(r, colMediaWAType, true)
	f.Status = readCode(r, "status", false)
	f.LegacySys = f.Type.OK && f.Type.Value == typeText && f.Status.OK && f.Status.Value == statusLegacySystem
	if b, ok := r.text("key_id"); ok {
		f.KeyID = cleanField(f.Raw, "guid", b)
	}
	f.Time = readInstant(r, "timestamp")
	f.Received = readInstant(r, "received_timestamp")
	f.Delivered = readInstant(r, "receipt_device_timestamp")
	if v, ok := r.int("receipt_server_timestamp"); ok {
		f.Raw["receipt_server_timestamp"] = v // legacy: milliseconds (research 7.12), raw only (R8)
	}
	r.rawInts(legacyRawInts()...)
	r.rawText("media_caption")
	if f.LegacySys {
		if v, ok := r.int("media_size"); ok {
			f.Raw["media_size"] = v // on a status 6 row it may be an event code (research 3.1): raw only
		}
	}

	if err := readLegacyChat(m, r, &f); err != nil {
		return msgFacts{}, err
	}
	if b, ok := r.text("remote_resource"); ok && len(b) > 0 {
		f.Sender = addrFromText(f.Raw, "remote_resource", b)
		f.Raw["remote_resource"] = cleanField(f.Raw, "remote_resource", b)
		if f.Sender.Str != "" {
			f.Raw["sender_jid"] = f.Sender.Str
		}
	} else {
		f.Sender = addr{State: stateAbsent}
	}
	if b, ok := r.text("data"); ok {
		f.Text, f.TextSource = b, textSourceText
		if l, ok := f.Type.label(); ok && l == labelContactCard && !f.LegacySys {
			f.TextSource = textSourcePart
		}
	}
	if err := readLegacyFeatures(m, r, &f); err != nil {
		return msgFacts{}, err
	}
	return f, nil
}

// readLegacyChat reads the chat of a legacy row: its JID is key_remote_jid as stored; its title is chat_list.subject
// and nothing else (4I-R13): a store without chat_list gives the thread an id and no title.
func readLegacyChat(m parse.MapContext, r cellReader, f *msgFacts) error {
	kb, ok := r.text("key_remote_jid")
	if !ok {
		f.Chat = addr{State: stateAbsent}
		return nil
	}
	if len(kb) == 0 {
		f.ChatState = "empty"
		return nil
	}
	f.Chat = addrFromText(f.Raw, "chat_jid", kb)
	rows, st, err := matchText(m, "chat_list", "key_remote_jid", kb)
	if err != nil {
		return err
	}
	switch st {
	case jUndecided:
		undecidedJoin(f.Raw, "chat_list")
	case jFound:
		switch len(rows) {
		case 0:
			f.Raw["chat_list_state"] = "no-chat_list-row"
		case 1:
			cr := cellReader{row: rows[0], cols: rowrun.Resolve(m, "chat_list", "subject"), raw: f.Raw, unknown: f.Unknown, prefix: "chat_list."}
			if b, ok := cr.text("subject"); ok && len(b) > 0 {
				f.Title = cleanField(f.Raw, "thread.title", b)
			}
		default:
			undecidedJoin(f.Raw, "chat_list") // more than one row for a key: never a choice
		}
	}
	return nil
}
