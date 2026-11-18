package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// channelWhatsApp is the channel of both apps (D3, D9): the app is raw.app.
const channelWhatsApp = "whatsapp"

// kindOf maps the type code to the message kind (spec 7.1): text, system, or media for every other mapped label; a
// code the research does not support, a revoke (a tombstone has no kind) and a legacy system row are unknown.
func kindOf(f *msgFacts) string {
	if f.LegacySys {
		return message.KindUnknown
	}
	switch l, ok := f.Type.label(); {
	case !ok, l == labelRevoked:
		return message.KindUnknown
	case l == labelText:
		return message.KindText
	case l == labelSystem:
		return message.KindSystem
	}
	return message.KindMedia
}

// build turns the facts of one row, from either generation, into the message record. It never knows the generation
// except to write raw.source_table (spec 6.2).
func build(m parse.MapContext, f msgFacts) ([]records.Record, error) {
	raw := f.Raw
	dp := classifyDB(m.Input().Primary.Logical)
	raw["source_table"], raw["app"], raw["app_basis"] = f.Table, dp.App, "path"
	msg := message.Message{Channel: channelWhatsApp, Direction: message.DirUnknown, Status: message.StatusUnknown, GUID: f.KeyID, Raw: raw}

	// direction and kind: the codes the research supports, the stored value always in raw
	if l, ok := f.Dir.label(); ok {
		msg.Direction = l
	}
	msg.Kind = kindOf(&f)
	if f.Reaction != nil {
		msg.Kind = message.KindReaction
	}
	if l, ok := f.Type.label(); ok && !f.LegacySys {
		raw["label_"+f.Type.Column] = l
	}

	// chat
	if f.Chat.Str != "" {
		raw["chat_jid"] = f.Chat.Str
	}
	if f.Chat.isStatus() {
		raw["chat_class"] = "status"
	}
	switch {
	case f.ChatState != "":
		raw["chat_state"] = f.ChatState
	case f.Chat.State == stateTooLong:
		raw["chat_state"] = stateTooLong
	}
	th := &message.Thread{ID: f.Chat.Str, Title: f.Title, Group: f.Chat.group()}
	if th.ID != "" || th.Title != "" || th.Group != nil {
		msg.Thread = th
	}

	if f.Reaction != nil {
		msg.TargetGUID = f.Reaction.Target
	}

	// participants, and the statements the database makes about the LIDs among them (D6)
	if f.Reaction != nil {
		// 4I-R20: no source of two establishes a reaction sender or direction: no participant, never a guess
		msg.Participants, msg.ParticipantsUnknown = nil, true
	} else {
		msg.Participants, msg.ParticipantsUnknown = participantsOf(&f, msg.Direction, raw)
	}
	if err := lidMappings(m, &f, msg.Participants); err != nil {
		return nil, err
	}

	rec := records.Record{Type: message.Type, ArtifactID: f.Place.ArtifactID, SourcePath: f.Place.SourcePath, Locator: f.Place.Locator, Range: &f.Place.Range}

	// a revoke is a tombstone: no body, no attachments, deleted by the app's own structure (spec 6.5). The text a revoked
	// row still holds (hostile or unexplained) is raw, cleaned, never the body.
	summaryText := ""
	if f.Revoke != nil {
		msg.Kind = message.KindUnknown
		msg.Deleted = map[string]any{"source": f.Revoke.Source}
		rec.Deleted = true
		if len(f.Text) > 0 {
			raw["revoked_row_text"] = cleanField(raw, "revoked_row_text", f.Text)
		}
		f.Text = nil
		summaryText = "[revoked, no content]"
	}

	// body
	if len(f.Text) > 0 {
		if cl := common.CleanText(f.Text, maxText); cl.Text != "" {
			rec.Body = cl.Text
			msg.TextSource = f.TextSource
			setBody(&msg, cl)
		}
	}

	// times: Time is the message's own timestamp; a sentinel or invalid value is no time, with its status in raw
	if f.Time != nil && f.Time.Valid() {
		rec.Time = &records.Time{T: f.Time.T}
	}
	if f.Received != nil && f.Received.Valid() {
		rec.Times = append(rec.Times, records.NamedTime{Kind: "received", Time: records.Time{T: f.Received.T}})
	}
	if f.Delivered != nil && f.Delivered.Valid() {
		rec.Times = append(rec.Times, records.NamedTime{Kind: "delivered", Time: records.Time{T: f.Delivered.T}})
	}

	if len(f.Unknown) > 0 {
		raw["unknown_columns"] = f.Unknown
	}
	counterparty := "unknown"
	switch {
	case len(msg.Participants) > 0:
		counterparty = msg.Participants[0].Address
	case f.Chat.Str != "":
		counterparty = f.Chat.Str
	}
	if summaryText == "" {
		summaryText = rec.Body
	}
	rec.Summary = message.Summary(channelWhatsApp, msg.Direction, counterparty, summaryText)
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}
