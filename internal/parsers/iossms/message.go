package iossms

import (
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const tableMessage = "message"

// messageSpecs are the columns of message the mapping reads, in the order of the DDL snapshot (the schema note
// lists the optional ones in this order). Required columns are the ones Probe insists on; everything else in the
// table stays in unmapped_columns.
func messageSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "ROWID", Required: true, Affinity: "integer"},
		{Name: "guid", Required: true, Affinity: "text"},
		{Name: "text", Affinity: "text"},
		{Name: "handle_id", Required: true, Affinity: "integer"},
		{Name: "subject", Affinity: "text"},
		{Name: "attributedBody", Affinity: "blob"},
		{Name: "service", Affinity: "text"},
		{Name: "error", Affinity: "integer"},
		{Name: "date", Required: true, Affinity: "integer"},
		{Name: "date_read", Affinity: "integer"},
		{Name: "date_delivered", Affinity: "integer"},
		{Name: "is_delivered", Affinity: "integer"},
		{Name: "is_from_me", Required: true, Affinity: "integer"},
		{Name: "is_read", Affinity: "integer"},
		{Name: "is_sent", Affinity: "integer"},
		{Name: "cache_roomnames", Affinity: "text"},
		{Name: "date_played", Affinity: "integer"},
		{Name: "item_type", Affinity: "integer"},
		{Name: "other_handle", Affinity: "integer"},
		{Name: "group_title", Affinity: "text"},
		{Name: "group_action_type", Affinity: "integer"},
		{Name: "associated_message_guid", Affinity: "text"},
		{Name: "associated_message_type", Affinity: "integer"},
		{Name: "balloon_bundle_id", Affinity: "text"},
		{Name: "payload_data", Affinity: "blob"},
		{Name: "expressive_send_style_id", Affinity: "text"},
		{Name: "message_summary_info", Affinity: "blob"},
		{Name: "date_retracted", Affinity: "integer"},
		{Name: "date_edited", Affinity: "integer"},
	}
}

func messageMapping() parse.TableMapping {
	return parse.TableMapping{Table: tableMessage, Columns: messageSpecs(), Map: mapMessage}
}

// rawInts are the integer columns stated in raw as stored (0 included).
func rawInts() []string {
	return []string{
		"handle_id", "is_from_me", "is_read", "is_sent", "is_delivered", "error", "item_type",
		"group_action_type", "associated_message_type", "other_handle",
	}
}

// rawTexts are the text columns stated in raw when they are not empty.
func rawTexts() []string {
	return []string{"group_title", "associated_message_guid", "balloon_bundle_id", "cache_roomnames", "expressive_send_style_id"}
}

// direction is is_from_me: 1 out, 0 in, anything else (or nothing readable) unknown.
func direction(v int64, ok bool) string {
	switch {
	case ok && v == 1:
		return message.DirOut
	case ok && v == 0:
		return message.DirIn
	}
	return message.DirUnknown
}

// channel is the service exactly (4E-R16); MMS is not inferred.
func channel(service string, ok bool) string {
	if ok {
		switch service {
		case "iMessage":
			return message.ChannelIMessage
		case "SMS":
			return message.ChannelSMS
		case "RCS":
			return "rcs"
		}
	}
	return "unknown"
}

// status: incoming is received; outgoing is failed (error != 0), delivered, sent, else unknown.
func status(dir string, v map[string]int64) string {
	switch dir {
	case message.DirIn:
		return message.StatusReceived
	case message.DirOut:
		switch {
		case v["error"] != 0:
			return message.StatusFailed
		case v["is_delivered"] == 1:
			return message.StatusDelivered
		case v["is_sent"] == 1:
			return message.StatusSent
		}
	}
	return message.StatusUnknown
}

// kind follows 4E-R17 and 4E-R41: a reaction type in [2000, 3999]; else an item or reaction type that is not a
// decided integer (NULL, another storage class, unreadable) makes the kind unknown; else item_type != 0 is a
// system item; else a body was read: text; else an attachment row: media; else unknown. A variant without the column means "not a reaction /
// not a system item".
func kind(assoc, item rowrun.CellKind, v map[string]int64, haveBody, haveAttachment bool) string {
	switch {
	case assoc == rowrun.CellValue && v["associated_message_type"] >= 2000 && v["associated_message_type"] <= 3999:
		return message.KindReaction
	case undecidedKind(assoc) || undecidedKind(item):
		return message.KindUnknown
	case item == rowrun.CellValue && v["item_type"] != 0:
		return message.KindSystem
	case haveBody:
		return message.KindText
	case haveAttachment:
		return message.KindMedia
	}
	return message.KindUnknown
}

func undecidedKind(k rowrun.CellKind) bool { return k != rowrun.CellValue && k != rowrun.CellMissing }

func mapMessage(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the message row has no rowid, so its chats and handle cannot be joined")
	}
	c := rowrun.Resolve(m, tableMessage, names(messageSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	msg := message.Message{Raw: raw}

	// integer columns, stated as stored; a NULL kind-deciding column is named
	v := map[string]int64{}
	kinds := map[string]rowrun.CellKind{}
	for _, col := range rawInts() {
		n, k := rowrun.IntCell(row, c.At(col))
		kinds[col] = k
		switch {
		case k == rowrun.CellValue:
			raw[col], v[col] = n, n
		case k == rowrun.CellNull && (col == "item_type" || col == "associated_message_type"):
			unknown[col] = "null"
		default:
			rowrun.Flag(unknown, row, col, c.At(col), k)
		}
	}
	_, haveFromMe := v["is_from_me"]
	msg.Direction = direction(v["is_from_me"], haveFromMe)
	msg.Status = status(msg.Direction, v)
	if rd, ok := v["is_read"]; ok && msg.Direction == message.DirIn && (rd == 0 || rd == 1) {
		b := rd == 1
		msg.Read = &b
	}

	// text columns
	svc, haveSvc := textCell(row, c, "service", "service", raw, unknown)
	msg.Channel = channel(svc, haveSvc)
	if svc != "" {
		msg.Service, raw["service"] = svc, svc
	}
	for _, col := range rawTexts() {
		if s, ok := textCell(row, c, col, col, raw, unknown); ok && s != "" {
			raw[col] = s
		}
	}
	msg.Subject, _ = textCell(row, c, "subject", "subject", raw, unknown)
	msg.GUID, _ = textCell(row, c, "guid", "guid", raw, unknown)
	if ag, ok := raw["associated_message_guid"].(string); ok {
		msg.TargetGUID = targetGUID(ag)
	}

	// body and kind
	b := readBody(m, row, c, raw, unknown)
	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if b.source != "" {
		rec.Body, msg.TextSource = b.cleaned.Text, b.source
		setBody(&msg, b.cleaned)
	}

	// attachments (4E-R19), then the kind that depends on them
	atts, err := attachments(m, id, raw, unknown)
	if err != nil {
		return nil, err
	}
	msg.Attachments = atts
	msg.Kind = kind(kinds["associated_message_type"], kinds["item_type"], v, b.source != "", len(atts) > 0)

	// participants and thread (4E-R20)
	if err := participants(m, &msg, id, v, raw, unknown); err != nil {
		return nil, err
	}

	// the plists (4E-R18): facts about their content, or a status and a warning
	bud := (*parse.BudgetView)(nil)
	if in := m.Input(); in != nil {
		bud = in.Budget
	}
	if blob, k := blobCell(row, c.At("message_summary_info")); k == rowrun.CellValue {
		summaryInfo(blob, bud, raw)
	} else {
		rowrun.Flag(unknown, row, "message_summary_info", c.At("message_summary_info"), k)
	}
	if blob, k := blobCell(row, c.At("payload_data")); k == rowrun.CellValue {
		payloadData(blob, bud, raw)
	} else {
		rowrun.Flag(unknown, row, "payload_data", c.At("payload_data"), k)
	}

	// times
	if t, ok := instant(row, c, "date", raw, unknown); ok {
		rec.Time = &records.Time{T: t}
	}
	rec.Times = secondaryTimes(row, c, raw, unknown)

	// the recently-deleted marker of the app (4E-R18)
	if err := recentlyDeleted(m, id, &rec, &msg, raw, unknown); err != nil {
		return nil, err
	}

	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	counterparty := ""
	if len(msg.Participants) > 0 {
		counterparty = msg.Participants[0].Address
	}
	rec.Summary = message.Summary(msg.Channel, msg.Direction, counterparty, rec.Body)
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}

// participants fills the participants and the thread of msg (4E-R20): incoming "from" is the handle of
// handle_id; outgoing "to" are the members of the only chat of the message, else the handle of handle_id; an
// unknown direction lists that handle as a member. Nothing is guessed: when the chat join is undecided, or the
// members of the only chat cannot be read, an outgoing message has unknown participants, never an empty list.
func participants(m parse.MapContext, msg *message.Message, id int64, v map[string]int64, raw, unknown map[string]any) error {
	ci, err := chatOf(m, id, raw, unknown)
	if err != nil {
		return err
	}
	msg.Thread = ci.thread

	var handle string
	if hid, ok := v["handle_id"]; ok && hid < 0 {
		raw["handle_state"] = "invalid-id"
	} else if ok && hid > 0 {
		a, err := handleAddress(m, hid)
		if err != nil {
			return err
		}
		handle = a.addr
		if a.addr == "" {
			raw["handle_state"] = a.state
		}
		if len(a.flags) > 0 {
			raw["handle_flags"] = anyStrings(a.flags)
			if a.rawB64 != "" {
				raw["handle_raw_b64"] = a.rawB64
			}
		}
	}
	switch msg.Direction {
	case message.DirIn:
		if handle != "" {
			msg.Participants = []message.Participant{participantOf(message.RoleFrom, handle)}
		}
	case message.DirOut:
		switch {
		case ci.undecidedJ || (ci.one && (!ci.membersOK || (ci.unresolved > 0 && len(ci.members) == 0))):
			// the recipients cannot be told
		case ci.one && len(ci.members) > 0:
			for _, a := range ci.members {
				msg.Participants = append(msg.Participants, participantOf(message.RoleTo, a))
			}
		case handle != "":
			msg.Participants = []message.Participant{participantOf(message.RoleTo, handle)}
		}
	default:
		if handle != "" {
			msg.Participants = []message.Participant{participantOf(message.RoleMember, handle)}
		}
	}
	// a group with members that could not be resolved lists the resolved ones but is incomplete: unknown, not absent
	msg.ParticipantsUnknown = len(msg.Participants) == 0 || (msg.Direction == message.DirOut && ci.one && ci.unresolved > 0)
	return nil
}

func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// recentlyDeleted reads the app's own Recently Deleted marker (4E-R18): a chat_recoverable_message_join row for
// the message makes it Deleted with {source: "chat_recoverable_message_join"}, Times.deleted from delete_date (a
// valid value only; the value and its unit are stated in raw.delete_date*) and raw.recoverable_chat_id. It is
// not a recovery: no Recovery, no Confidence. A database without the table has no such marker; an undecided
// join lists the table in raw.join_undecided. With several rows (not expected: the key is chat and message) the
// first in rowid order is read and raw.recoverable_rows counts them.
func recentlyDeleted(m parse.MapContext, id int64, rec *records.Record, msg *message.Message, raw, unknown map[string]any) error {
	rows, undecided, err := rowrun.Match(m, "chat_recoverable_message_join", "message_id", id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return nil
	case err != nil:
		return err
	}
	if undecided {
		markUndecided(raw, "chat_recoverable_message_join")
	}
	if len(rows) == 0 {
		return nil
	}
	row := rows[0]
	c := rowrun.Resolve(m, "chat_recoverable_message_join", "chat_id", "delete_date")
	rec.Deleted, msg.Deleted = true, map[string]any{"source": "chat_recoverable_message_join"}
	if cid, k := rowrun.IntCell(row, c.At("chat_id")); k == rowrun.CellValue {
		raw["recoverable_chat_id"] = cid
	} else {
		rowrun.Flag(unknown, row, "chat_recoverable_message_join.chat_id", c.At("chat_id"), k)
	}
	if t, ok := instant(row, c, "delete_date", raw, unknown); ok {
		rec.Times = append(rec.Times, records.NamedTime{Kind: "deleted", Time: records.Time{T: t}})
	}
	if len(rows) > 1 {
		raw["recoverable_rows"] = int64(len(rows))
	}
	return nil
}
