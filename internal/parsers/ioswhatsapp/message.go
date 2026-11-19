package ioswhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// channelWhatsApp is the channel of both apps (D3, D9): the app is raw.app.
const channelWhatsApp = "whatsapp"

const (
	tableMember = "ZWAGROUPMEMBER"
	tableInfo   = "ZWAMESSAGEINFO"
)

// env is what Parse hands the mapping: the place of the database and its app label. It is nil in Mappings(), the typed
// recovery pass, which has no Parse context (4I2-P8, PF5): the mapping then derives the label from the Input of its
// MapContext.
type env struct {
	Place dbPlace
	Label appLabel
}

// messageSpecs is the ZWAMESSAGE column list; the Required columns are the ones Probe insists on.
func messageSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "Z_PK", Required: true, Affinity: "integer"},
		{Name: "ZCHATSESSION", Required: true, Affinity: "integer"},
		{Name: "ZISFROMME", Required: true, Affinity: "integer"},
		{Name: "ZMESSAGEDATE", Required: true, Affinity: "real"},
		{Name: "ZMESSAGETYPE", Required: true, Affinity: "integer"},
		{Name: "Z_ENT", Affinity: "integer"},
		{Name: "ZGROUPEVENTTYPE", Affinity: "integer"},
		{Name: "ZMESSAGESTATUS", Affinity: "integer"},
		{Name: "ZMESSAGEERRORSTATUS", Affinity: "integer"},
		{Name: "ZSORT", Affinity: "integer"},
		{Name: "ZFLAGS", Affinity: "integer"},
		{Name: "ZSTARRED", Affinity: "integer"},
		{Name: "ZGROUPMEMBER", Affinity: "integer"},
		{Name: "ZMEDIAITEM", Affinity: "integer"},
		{Name: "ZMESSAGEINFO", Affinity: "integer"},
		{Name: "ZSENTDATE", Affinity: "real"},
		{Name: "ZFROMJID", Affinity: "text"},
		{Name: "ZTOJID", Affinity: "text"},
		{Name: "ZPUSHNAME", Affinity: "text"},
		{Name: "ZSTANZAID", Affinity: "text"},
		{Name: "ZTEXT", Affinity: "text"},
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// messageMappingWith maps ZWAMESSAGE with the environment e (nil: the typed recovery pass): a closure over an immutable
// value, so the mapper stays a pure function of the row and its context.
func messageMappingWith(e *env) parse.TableMapping {
	names := specNames(messageSpecs())
	return parse.TableMapping{Table: tableMessage, Columns: messageSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return mapMessage(m, row, e, names)
	}}
}

// rowLabel is the app label of the rows of this mapping: Parse's when it has one, else derived from the place (PF5):
// a domain names its app; a recovered row of an app group states "unknown" and "not-evaluated" (4I2-P8).
func rowLabel(m parse.MapContext, e *env) appLabel {
	if e != nil {
		return e.Label
	}
	if p := classifyPlace(m.Input().Primary.Logical); p.Place == placeDomain {
		return appLabel{App: p.App, Basis: "backup_domain"}
	}
	return appLabel{App: "unknown", State: "not-evaluated"}
}

// chatFacts is the chat session of a message. Ref is the chat JID (its State names why there is none, in the vocabulary
// of raw.chat_state); Type is ZWACHATSESSION.ZSESSIONTYPE as read.
type chatFacts struct {
	Ref   jidRef
	Type  code
	Title string
}

// group and status say what the chat is: the session type or the JID's class says so (4I2-P5); neither alone is trusted
// to say "not".
func (c chatFacts) group() bool {
	return (c.Type.OK && c.Type.Value == sessionGroup) || (c.Ref.usable() && c.Ref.class() == string(wajid.ClassGroup))
}

func (c chatFacts) status() bool {
	return (c.Type.OK && c.Type.Value == sessionStatus) || (c.Ref.usable() && c.Ref.class() == string(wajid.ClassStatus))
}

// readChat joins the chat session of the message and reads its JID. A dangling key is stated as raw.chat_state; a join
// that cannot be decided is listed in raw.join_undecided; neither is read as a chat with no JID.
func readChat(m parse.MapContext, r cellReader, raw, unknown map[string]any) (chatFacts, error) {
	var c chatFacts
	col := r.cols.At("ZCHATSESSION")
	key, k := intCellOf(r.row, col)
	switch {
	case k == rowrun.CellNull || (k == rowrun.CellValue && key == 0):
		c.Ref.State = "absent"
		return c, nil
	case k != rowrun.CellValue:
		rowrun.Flag(unknown, r.row, prefixMessage+"ZCHATSESSION", col, k)
		c.Ref.State = stateUndecided
		return c, nil
	}
	srow, st, err := getRow(m, tableChat, key)
	if err != nil {
		return c, err
	}
	switch st {
	case jNoRow:
		c.Ref.State = "no-ZWACHATSESSION-row"
		return c, nil
	case jUndecided:
		undecidedJoin(raw, tableChat)
		c.Ref.State = stateUndecided
		return c, nil
	}
	sr := cellReader{row: srow, cols: rowrun.Resolve(m, tableChat, colSessionType, "ZREMOVED", "ZHIDDEN", "ZFLAGS", "ZPARTNERNAME", "ZCONTACTJID"), raw: raw, unknown: unknown, prefix: prefixSession}
	sess := map[string]any{}
	for _, n := range []string{colSessionType, "ZREMOVED", "ZHIDDEN", "ZFLAGS"} {
		if v, ok := sr.int(n); ok {
			sess[n] = v
		}
	}
	if len(sess) > 0 {
		raw["session"] = sess
	}
	c.Type.Column = colSessionType
	c.Type.Value, c.Type.OK = sr.int(colSessionType)
	if l, ok := c.Type.label(); ok {
		raw["label_"+colSessionType] = l
	}
	jid := readJID(srow, sr.cols.At("ZCONTACTJID"), raw, unknown, "chat_jid")
	switch jid.State {
	case "":
		raw["chat_jid"] = jid.Text
		raw["chat_class"] = jid.class()
	case stateEmpty:
		jid.State = "no-contact-jid"
	}
	c.Ref = jid
	if c.Type.OK && c.Type.Value == sessionGroup {
		if b, ok := sr.text("ZPARTNERNAME"); ok && len(b) > 0 {
			c.Title = cleanField(raw, "thread.title", b)
		}
	}
	return c, nil
}

// readSender joins the author of an incoming group message: ZWAMESSAGE.ZGROUPMEMBER names a ZWAGROUPMEMBER row whose
// ZMEMBERJID is the sender. The returned State is "" for a usable person's JID, else why there is none (the vocabulary
// of raw.sender_state).
func readSender(m parse.MapContext, r cellReader, raw, unknown map[string]any) (jidRef, error) {
	key, k := intCellOf(r.row, r.cols.At("ZGROUPMEMBER"))
	switch {
	case k == rowrun.CellNull || (k == rowrun.CellValue && key == 0):
		return jidRef{State: "absent"}, nil
	case k != rowrun.CellValue:
		return jidRef{State: stateUndecided}, nil // the cell was flagged when it was read as raw
	}
	mrow, st, err := getRow(m, tableMember, key)
	if err != nil {
		return jidRef{}, err
	}
	switch st {
	case jNoRow:
		return jidRef{State: "no-ZWAGROUPMEMBER-row"}, nil
	case jUndecided:
		undecidedJoin(raw, tableMember)
		return jidRef{State: stateUndecided}, nil
	}
	j := readJID(mrow, m.Column(tableMember, "ZMEMBERJID"), raw, unknown, "member_jid_text")
	switch {
	case j.State == stateEmpty:
		j.State = "no-member-jid"
	case j.usable():
		switch j.class() {
		case string(wajid.ClassGroup), string(wajid.ClassBroadcast), string(wajid.ClassStatus):
			j.State = "not-a-person"
		}
	}
	return j, nil
}

// fileType says whether the ZMESSAGETYPE value is one whose media row is an attachment (4I2-P6, spec 6.7's MIME list):
// image, video, audio, document, gif, sticker. Types 4, 5 and 7 join their media row for the body or raw only.
func fileType(v int64) bool {
	switch v {
	case 1, 2, 3, 8, 11, 15:
		return true
	}
	return false
}

// joinsMedia says whether the message type joins its media row: every file type, a contact card, a location, a link and a
// revoke (whose row keeps the vCard name, spec 6.11).
func joinsMedia(v int64) bool {
	return fileType(v) || v == typeContactCard || v == typeLocation || v == typeLink || v == typeRevoked
}

// kindOf maps the type label to the message kind (spec 7.1): text, system, or media for every other mapped label; a code
// the research does not support, and a revoke (a tombstone has no kind), are unknown.
func kindOf(typ code) string {
	switch l, ok := typ.label(); {
	case !ok, l == labelRevoked:
		return message.KindUnknown
	case l == labelText:
		return message.KindText
	case l == labelGroupEvent, l == labelSysNotice:
		return message.KindSystem
	}
	return message.KindMedia
}

// mapMessage turns one ZWAMESSAGE row and its joins into the message record (spec 6.7, 6.11, 7; 4I2-R7).
func mapMessage(m parse.MapContext, row parse.Row, e *env, names []string) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	raw, unknown := map[string]any{}, map[string]any{}
	rowLabel(m, e).raw(raw)
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl
	}
	r := cellReader{row: row, cols: rowrun.Resolve(m, tableMessage, names...), raw: raw, unknown: unknown, prefix: prefixMessage}
	pk, hasPK := row.Rowid()

	dir := readCode(r, colIsFromMe)
	typ := readCode(r, colMessageType)
	gev := readCode(r, colGroupEventType)
	stat := readCode(r, colMessageStatus)
	for _, n := range []string{"ZMESSAGEERRORSTATUS", "ZSORT", "ZFLAGS", "ZSTARRED", "Z_ENT"} {
		if v, ok := r.int(n); ok {
			raw[n] = v
		}
	}
	if n, ok := r.number("ZSENTDATE"); ok {
		raw["ZSENTDATE"] = n.Raw() // meaning not established (research conflict 8): raw only, never a time
	} else {
		r.nanStatus("ZSENTDATE")
	}
	for _, n := range []string{"ZFROMJID", "ZTOJID", "ZPUSHNAME"} {
		if b, ok := r.text(n); ok {
			raw[n] = cleanField(raw, n, b)
		}
	}
	if v, ok := r.int("ZGROUPMEMBER"); ok {
		raw["ZGROUPMEMBER"] = v
	}
	msg := message.Message{Channel: channelWhatsApp, Direction: message.DirUnknown, Status: message.StatusUnknown, Raw: raw}
	if b, ok := r.text("ZSTANZAID"); ok && len(b) > 0 {
		msg.GUID = cleanField(raw, "guid", b)
	}

	// direction, kind and labels: the codes two sources agree about, the stored value always in raw
	if l, ok := dir.label(); ok {
		msg.Direction = l
	}
	msg.Kind = kindOf(typ)
	if l, ok := typ.label(); ok {
		raw["label_"+colMessageType] = l
		if typ.Value == typeGroupEvent {
			if gl, ok := gev.label(); ok {
				raw["label_"+colGroupEventType] = gl
			}
		}
	}
	if l, ok := stat.label(); ok && msg.Direction == message.DirOut {
		msg.Status = message.StatusDelivered // read is never set from it (spec 7.3)
		raw["label_"+colMessageStatus] = l
	}
	revoke := typ.OK && typ.Value == typeRevoked

	// time: Cocoa seconds, a REAL or an INTEGER; NULL is no time and a non-number is stated, never a time
	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if n, ok := r.number("ZMESSAGEDATE"); ok {
		in := cellx.CocoaSeconds(n)
		in.Record(raw, "ZMESSAGEDATE")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
		}
	} else {
		r.nanStatus("ZMESSAGEDATE")
	}

	// the chat and who the message is from or to
	chat, err := readChat(m, r, raw, unknown)
	if err != nil {
		return nil, err
	}
	if chat.Ref.State != "" {
		raw["chat_state"] = chat.Ref.State
	}
	group, status := chat.group(), chat.status()
	var sender jidRef
	if msg.Direction == message.DirIn && group && !status {
		if sender, err = readSender(m, r, raw, unknown); err != nil {
			return nil, err
		}
		if sender.State != "" {
			raw["sender_state"] = sender.State
		}
	}
	msg.Participants, msg.ParticipantsUnknown = participantsOf(msg.Direction, chat.Ref, group, status, sender)
	addressConflict(r, raw, msg.Direction, chat, group, status)
	th := &message.Thread{Title: chat.Title}
	if chat.Ref.usable() {
		th.ID = chat.Ref.Text
	}
	if _, mapped := chat.Type.label(); mapped {
		g := chat.Type.Value == sessionGroup // thread.group is true for ZSESSIONTYPE 1 only, absent for an unknown type
		th.Group = &g
	}
	if th.ID != "" || th.Title != "" || th.Group != nil {
		msg.Thread = th
	}

	// the body and the media row: ZTEXT; else the media row's ZTITLE (file types) or the vCard (type 4)
	var text []byte
	if b, ok := r.text("ZTEXT"); ok {
		text = b
	}
	textSource := message.TextSourceText
	if typ.OK && joinsMedia(typ.Value) {
		item, j, err := joinMediaOf(m, r, raw, pk, hasPK)
		if err != nil {
			return nil, err
		}
		if j == mediaRow {
			text, textSource = readMediaRow(mediaReader(m, item, raw, unknown), typ.Value, text, textSource)
			if fileType(typ.Value) {
				if a, ok := attachmentOf(m, mediaReader(m, item, raw, unknown)); ok {
					msg.Attachments = append(msg.Attachments, a)
				}
			}
		} else if j == mediaNone && fileType(typ.Value) {
			raw["media_state"] = stateNoMediaRow
		}
	}

	// a revoke is a tombstone: no body, no attachment, deleted by the app's own structure (spec 6.5). The text a revoked
	// row still holds (hostile or unexplained) is raw, cleaned, never the body.
	summaryText := ""
	if revoke {
		msg.Kind = message.KindUnknown
		msg.Deleted = map[string]any{"source": colMessageType}
		rec.Deleted = true
		if len(text) > 0 {
			raw["revoked_row_text"] = cleanField(raw, "revoked_row_text", text)
		}
		text = nil
		summaryText = "[revoked, no content]"
	}
	if len(text) > 0 {
		if cl := common.CleanText(text, maxText); cl.Text != "" {
			rec.Body = cl.Text
			msg.TextSource = textSource
			setBody(&msg, raw, cl)
		}
	}
	if err := readReceipt(m, r, raw, unknown); err != nil {
		return nil, err
	}

	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	counterparty := "unknown"
	switch {
	case len(msg.Participants) > 0:
		counterparty = msg.Participants[0].Address
	case chat.Ref.usable():
		counterparty = chat.Ref.Text
	}
	if summaryText == "" {
		summaryText = rec.Body
	}
	rec.Summary = message.Summary(channelWhatsApp, msg.Direction, counterparty, summaryText)
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}

// addressConflict states raw.address_conflict: the stored ZFROMJID of an incoming row (ZTOJID of an outgoing one) that
// disagrees byte-wise with the chat JID of a private (ZSESSIONTYPE 0) 1:1 chat, never resolved (R3). Never stated on a
// group or status row, where the stored JID is the group's by design; an altered stored JID is no address to compare.
func addressConflict(r cellReader, raw map[string]any, dir string, chat chatFacts, group, status bool) {
	if !chat.Type.OK || chat.Type.Value != 0 || group || status || !chat.Ref.usable() {
		return
	}
	col := "ZFROMJID"
	if dir == message.DirOut {
		col = "ZTOJID"
	} else if dir != message.DirIn {
		return
	}
	b, k := textCellOf(r.row, r.cols.At(col))
	if k != rowrun.CellValue || len(b) == 0 {
		return
	}
	if cl := common.CleanText(b, maxText); len(cl.Flags) == 0 && cl.Text != chat.Ref.Text {
		raw["address_conflict"] = []any{col}
	}
}

// readMediaRow takes what the media row gives a message: for a revoke only the vCard name (the row is a tombstone's
// leftover); otherwise the body (the vCard of a contact card, the title of a file type with no ZTEXT, each with
// raw.body_column and text_source "part") and the media fields. It returns the body bytes and their source.
func readMediaRow(ir cellReader, typ int64, text []byte, source string) ([]byte, string) {
	if typ == typeRevoked {
		if b, ok := ir.text("ZVCARDNAME"); ok {
			ir.raw["ZVCARDNAME"] = cleanField(ir.raw, "ZVCARDNAME", b)
		}
		return text, source
	}
	titleIsBody := false
	switch {
	case typ == typeContactCard:
		if b, ok := ir.text("ZVCARDSTRING"); ok && len(b) > 0 {
			text, source = b, message.TextSourcePart
			ir.raw["body_column"] = "ZVCARDSTRING"
		}
	case fileType(typ) && len(text) == 0:
		if b, ok := ir.text("ZTITLE"); ok && len(b) > 0 {
			text, source, titleIsBody = b, message.TextSourcePart, true
			ir.raw["body_column"] = "ZTITLE"
		}
	}
	readMediaFields(ir, typ, titleIsBody)
	return text, source
}

// joinMediaOf makes the media join of a message row: a row whose rowid is lost (a recovered row) cannot be matched, so the
// join is undecided; an undecided join is listed in raw.join_undecided, never read as "no media row".
func joinMediaOf(m parse.MapContext, r cellReader, raw map[string]any, pk int64, hasPK bool) (parse.Row, mediaJoin, error) {
	if !hasPK {
		undecidedJoin(raw, tableMediaItem)
		return nil, mediaUndecided, nil
	}
	item, j, err := joinMedia(m, pk, mediaKeyOf(r))
	if err != nil {
		return nil, j, err
	}
	if j == mediaUndecided {
		undecidedJoin(raw, tableMediaItem)
	}
	return item, j, nil
}

// readReceipt states the receipt blob of ZWAMESSAGEINFO by length and SHA-256 (spec 8: never decoded): ZMESSAGEINFO
// names the row. A key that names no row is raw.receipt_info_state; a table that cannot be joined is raw.join_undecided.
func readReceipt(m parse.MapContext, r cellReader, raw, unknown map[string]any) error {
	col := r.cols.At("ZMESSAGEINFO")
	key, k := intCellOf(r.row, col)
	rowrun.Flag(unknown, r.row, prefixMessage+"ZMESSAGEINFO", col, k)
	if k != rowrun.CellValue || key == 0 {
		return nil // NULL and 0 name none; an unreadable cell is stated in unknown_columns, never read as none
	}
	irow, st, err := getRow(m, tableInfo, key)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		raw["receipt_info_state"] = "no-ZWAMESSAGEINFO-row"
		return nil
	case jUndecided:
		undecidedJoin(raw, tableInfo)
		return nil
	}
	ir := cellReader{row: irow, cols: rowrun.Resolve(m, tableInfo, "ZRECEIPTINFO"), raw: raw, unknown: unknown, prefix: prefixInfo}
	if b, ok := ir.blob("ZRECEIPTINFO"); ok {
		raw["receipt_info"] = digest(b)
	}
	return nil
}
