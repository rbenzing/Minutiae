package androidtelegram

import (
	"encoding/binary"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// env is what a live parse gives a mapping (4I4-P6): the join guard and the account of the path. A nil *env is the
// typed recovery pass: a recovered row joins nothing and carries no account.
type env struct {
	Guard   joinGuard
	Account string
}

// The fixed values of the message mapping (spec 8.3-8.5).
const (
	channelTelegram     = "telegram"
	dialogBasisPre84    = "pre-84-high-id"
	messageIDBasisPre84 = "pre-84-channel-packing"
	stateUndecided      = "undecided"
	stateOutOfRange     = "out-of-range"
	ttlStatePre84       = "pre-84-forward-peer"
	labelMessageEmpty   = "messageEmpty"
	unknownNull         = "null" // unknown_columns of a NULL cell the record cannot do without (spec 12; as ios.sms)
	unknownTypeMismatch = "type-mismatch"
	idMediaEmpty        = 0x3ded6320 // TL_messageMediaEmpty: a message without media (spec 8.5)
)

// plainColumn is one integer column kept in raw as stored (spec 8.5 raw list), under its raw key.
type plainColumn struct{ col, key string }

// plainColumns are the integer columns of the message tables kept as stored, in the order of the plan's vocabulary;
// a column the variant lacks is skipped (v64's messages has no forwards, ...).
func plainColumns() []plainColumn {
	return []plainColumn{
		{"mid", "mid"},
		{"uid", "uid"},
		{"out", "out"},
		{"read_state", "read_state"},
		{"send_state", "send_state"},
		{"ttl", "ttl"},
		{"media", "views_or_media_flag"},
		{"imp", "imp"},
		{"mention", "mention"},
		{"forwards", "forwards"},
		{"thread_reply_id", "thread_reply_id"},
		{"is_channel", "is_channel"},
		{"reply_to_message_id", "reply_to_message_id"},
		{"group_id", "group_id"},
		{"reply_to_story_id", "reply_to_story_id"},
	}
}

// keyColumns are the plain columns the record cannot do without (spec 12): a NULL there is undecided, listed in
// unknown_columns as "null", never kept as a stored NULL.
func keyColumns() []string { return []string{"mid", "uid", "date", "out"} }

// messageBools are the blob's booleans kept in raw when set (spec 8.5, recon 3.2).
func messageBools() []string {
	return []string{
		"silent", "post", "from_scheduled", "legacy", "edit_hide", "pinned", "noforwards", "offline", "invert_media",
		"mentioned", "media_unread", "unread",
	}
}

// messageColumns are the columns a message mapping reads: the signature columns of spec 8.1 are required, every other
// is optional (C3), so the v64 messages table reads with fewer.
func messageColumns() []parse.ColumnSpec {
	req := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Required: true, Affinity: aff} }
	opt := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Affinity: aff} }
	return []parse.ColumnSpec{
		req("mid", "integer"), req("uid", "integer"), req("date", "integer"), req("data", "blob"), req("out", "integer"),
		opt("read_state", "integer"), opt("send_state", "integer"), opt("ttl", "integer"), opt("media", "integer"),
		opt("replydata", "blob"), opt("imp", "integer"), opt("mention", "integer"), opt("forwards", "integer"),
		opt("replies_data", "blob"), opt("thread_reply_id", "integer"), opt("is_channel", "integer"),
		opt("reply_to_message_id", "integer"), opt("custom_params", "blob"), opt("group_id", "integer"),
		opt("reply_to_story_id", "integer"),
	}
}

// messageMappingWith is the mapping of the message table ("messages" or "messages_v2"): one record per row (spec
// 8.3-8.5). A nil e is the typed recovery pass (4I4-P6). It builds its own constructor table; Parse, which runs two
// mappings, builds one table for both through messageMapping (C2: once per Parse, never per row).
func messageMappingWith(e *env, table string) parse.TableMapping {
	return messageMapping(e, newTable(), table)
}

// messageMapping is messageMappingWith over the constructor table tab.
func messageMapping(e *env, tab tl.Table, table string) parse.TableMapping {
	cols := messageColumns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return parse.TableMapping{Table: table, Columns: cols, Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		if e != nil && len(e.Guard.bad) > 0 {
			m = guardedCtx{MapContext: m, g: &e.Guard}
		}
		return mapMessage(m, row, e, tab, table, rowrun.Resolve(m, table, names...))
	}}
}

// nanCell says a cell holds a stored NaN, which SQLite reads as NULL: it is a value of another class, never NULL
// (4I2-A6; restated from androidcalendar/times.go, F29).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// intCellOf is rowrun.IntCell with a stored NaN read as a value of another class.
func intCellOf(row parse.Row, col int) (int64, rowrun.CellKind) {
	v, k := rowrun.IntCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return v, k
}

// intCell is one integer cell as read: its value when ok.
type intCell struct {
	v  int64
	ok bool
}

// msgRow holds what the mapping of one row reads before it builds the record.
type msgRow struct {
	table        string
	raw, unknown map[string]any
	cells        map[string]intCell
}

// unknownName is the unknown_columns key of a column: "<table>.<col>".
func (r *msgRow) unknownName(col string) string { return r.table + "." + col }

// readPlain reads the plain integer columns into raw (as stored, NULL included) and r.cells; a cell of another
// class or an unreadable one is listed in unknown_columns, a NULL key column as "null" (spec 12).
func (r *msgRow) readPlain(row parse.Row, c rowrun.Cols) {
	for _, p := range plainColumns() {
		col := c.At(p.col)
		if col < 0 {
			continue
		}
		v, k := intCellOf(row, col)
		switch k {
		case rowrun.CellValue:
			r.raw[p.key] = v
			r.cells[p.col] = intCell{v: v, ok: true}
		case rowrun.CellNull:
			if slices.Contains(keyColumns(), p.col) {
				r.unknown[r.unknownName(p.col)] = unknownNull
				continue
			}
			r.raw[p.key] = nil // a stored NULL is a fact about the row (C1)
		case rowrun.CellOther:
			r.unknown[r.unknownName(p.col)] = unknownTypeMismatch
		case rowrun.CellUnknown:
			r.unknown[r.unknownName(p.col)] = rowrun.StateOf(row, col).String()
		}
	}
}

// cell is a plain cell read by readPlain.
func (r *msgRow) cell(col string) (int64, bool) {
	c := r.cells[col]
	return c.v, c.ok
}

// readDate reads the date column (spec 10): a valid Unix-seconds value is the record time; a sentinel or an invalid
// value states raw.date_status; a NULL or unreadable cell is listed in unknown_columns. The blob's own date is
// raw.date (readBlobRaw).
func (r *msgRow) readDate(row parse.Row, c rowrun.Cols) (*records.Time, int64, bool) {
	col := c.At("date")
	if col < 0 {
		return nil, 0, false
	}
	v, k := intCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		inst := rowrun.Seconds(v)
		if inst.Status != ts.StatusValid {
			r.raw["date_status"] = inst.Status.String()
			return nil, v, true
		}
		return &records.Time{T: inst.T, Basis: records.BasisUTC}, v, true
	case rowrun.CellNull:
		r.unknown[r.unknownName("date")] = unknownNull
	case rowrun.CellOther:
		r.unknown[r.unknownName("date")] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[r.unknownName("date")] = rowrun.StateOf(row, col).String()
	}
	return nil, 0, false
}

// readPre84 states the pre-84 conventions of a messages row (spec 8.3, facts.tsv predicate.pre84_channel_mid,
// ttl): a channel row (negative uid) whose mid packs the channel id in its high 32 bits gives raw.message_id (the
// low 32 bits as a signed int) with its basis; one whose mid does not is raw.mid_state "undecided" (never a
// positive-uid row, F32); a negative ttl is a stored forward peer, not a lifetime. It returns the message id the
// blob's id is compared with.
func (r *msgRow) readPre84() (int64, bool) {
	mid, midOK := r.cell("mid")
	if r.table != tableMessages {
		return mid, midOK
	}
	uid, uidOK := r.cell("uid")
	id := mid
	if midOK && uidOK && uid < 0 {
		if mid>>32 == -uid {
			id = int64(int32(mid)) // the pinned predicate keeps the low 32 bits as a signed int
			r.raw["message_id"], r.raw["message_id_basis"] = id, messageIDBasisPre84
		} else {
			r.raw["mid_state"] = stateUndecided
		}
	}
	if ttl, ok := r.cell("ttl"); ok && ttl < 0 {
		r.raw["ttl_state"] = ttlStatePre84
	}
	return id, midOK
}

// readBlobRaw states the decoded blob fields of spec 8.5's raw list (the media, forward header and trailer are
// mediaSummary's, fwdRaw's and readTrailer's, the reply copy replyCopy's, the peer peerRaw's): the flags words,
// the set booleans, the id (and id_conflict against the message id), via_bot_id, grouped_id, ttl_period, views,
// reply_to.reply_to_msg_id, edit_date with its unit, the blob date (and date_conflict against the date column) and the
// messageEmpty label.
func readBlobRaw(raw map[string]any, v view, c tl.Constructor, msgID int64, msgIDOK bool, date int64, dateOK bool) {
	if f, ok := v.Int("flags"); ok {
		raw["blob_flags"] = f
	}
	if f, ok := v.Int("flags2"); ok {
		raw["blob_flags2"] = f
	}
	for _, name := range messageBools() {
		if v.True(name) {
			raw[name] = true
		}
	}
	if id, ok := v.Int("id"); ok {
		raw["id"] = id
		if msgIDOK && id != msgID {
			raw["id_conflict"] = true
		}
	}
	for _, key := range []string{"via_bot_id", "grouped_id", "ttl_period", "views"} {
		if x, ok := v.Int(key); ok {
			raw[key] = x
		}
	}
	if x, ok := v.Int("reply_to.reply_to_msg_id"); ok {
		raw["reply_to_msg_id"] = x
	}
	if x, ok := v.Int("edit_date"); ok {
		// raw only: Times.edited would need unitEditDate "seconds", and the pinned source leaves the unit
		// unestablished (facts.tsv unit.edit_date; TestTelegramFactsMatchTheCode)
		raw["edit_date"], raw["edit_date_unit"] = x, unitEditDate
	}
	if x, ok := v.Int("date"); ok {
		raw["date"] = x
		if dateOK && x != date {
			raw["date_conflict"] = true
		}
	}
	if strings.HasPrefix(c.Name, "TL_messageEmpty") {
		raw["label_constructor"] = labelMessageEmpty
	}
}

// directionOf is the direction of spec 8.5: the out column (0 in, 1 out, else unknown); a decoded blob that
// disagrees (column 0 with the blob's out set; column 1 with neither out nor from_scheduled) makes it unknown with
// raw.direction_conflict. The blob is asked only when its constructor has an out field and its flags word (or its
// out bool) was read.
func directionOf(raw map[string]any, out int64, outOK bool, v view, c tl.Constructor, top bool) string {
	dir := message.DirUnknown
	switch {
	case outOK && out == 0:
		dir = message.DirIn
	case outOK && out == 1:
		dir = message.DirOut
	}
	if !top || dir == message.DirUnknown || !hasField(c, "out") || (!v.Has("flags") && !v.Has("out")) {
		return dir
	}
	blobOut, sched := v.True("out"), v.True("from_scheduled")
	if (dir == message.DirIn && blobOut) || (dir == message.DirOut && !blobOut && !sched) {
		raw["direction_conflict"] = true
		return message.DirUnknown
	}
	return dir
}

func hasField(c tl.Constructor, name string) bool {
	for _, f := range c.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// statusOf is the status of spec 8.5 from send_state (the writer's constants MO:138-141): outgoing 0 sent,
// 1 queued (label sending), 2 failed, 3 unknown (label editing), another value unknown; incoming 0 received, another
// value unknown; an unknown direction unknown. A send_state not read gives no status.
func statusOf(raw map[string]any, dir string, send int64, ok bool) string {
	if !ok {
		return ""
	}
	switch dir {
	case message.DirOut:
		switch send {
		case 0:
			return message.StatusSent
		case 1:
			raw["label_send_state"] = "sending"
			return message.StatusQueued
		case 2:
			return message.StatusFailed
		case 3:
			raw["label_send_state"] = "editing"
		}
	case message.DirIn:
		if send == 0 {
			return message.StatusReceived
		}
	}
	return message.StatusUnknown
}

// readOf is read of spec 8.5: read_state bit0 for an incoming row whose value is 0..3; an outgoing row keeps
// read_state raw only (what bit0 means there is from memory); a value outside 0..3 is raw.read_state_state
// "out-of-range".
func readOf(raw map[string]any, dir string, rs int64, ok bool) *bool {
	switch {
	case !ok:
		return nil
	case rs < 0 || rs > 3:
		raw["read_state_state"] = stateOutOfRange
		return nil
	case dir != message.DirIn:
		return nil
	}
	read := rs&1 == 1
	return &read
}

// mediaOf says what the blob holds as media (spec 8.5): "empty" (messageMediaEmpty, or a media field gated off by
// a flags word that was read), "media" (another media object, or an opaque one the decode stopped at), "" when
// undecided (a media ID the table lacks, or the media not reached).
func mediaOf(v view, c tl.Constructor) string {
	if id, ok := v.ID("media"); ok {
		if id == idMediaEmpty {
			return "empty"
		}
		return "media"
	}
	if s := v.stopAt("media"); s != nil {
		if s.Kind == tl.StopOpaque {
			return "media" // a media constructor the table knows by its ID only
		}
		return ""
	}
	for _, f := range c.Fields {
		if f.Name != "media" || f.Flags == "" {
			continue
		}
		if word, ok := v.Int(f.Flags); ok && word>>uint(f.Bit)&1 == 0 {
			return "empty"
		}
	}
	return ""
}

// kindOf is the kind of spec 8.5 from the top constructor: TL_messageEmpty* unknown (raw.label_constructor);
// TL_messageService* system when its action was reached (mapMessage turns a phone-call action into a call, spec 8.7; a
// conference call and every other action stay system messages); another message text without media or with
// messageMediaEmpty, media with another media; a blob with no known top, or whose media is undecided, unknown. A
// truncated, malformed, capped or refused blob takes the same rule when its stop lies after media (or the action), and
// is unknown when the stop is at or before it (spec 8.4: the fields before a stop are as partial; review I1).
func kindOf(v view, c tl.Constructor, top bool, state string) string {
	if !top {
		return message.KindUnknown
	}
	if state != stateDecoded && state != statePartial {
		decided := "media"
		if strings.HasPrefix(c.Name, "TL_messageService") {
			decided = "action"
		}
		if !stopAfter(v, c, decided) {
			return message.KindUnknown
		}
	}
	switch {
	case strings.HasPrefix(c.Name, "TL_messageEmpty"):
		return message.KindUnknown
	case strings.HasPrefix(c.Name, "TL_messageService"):
		if v.Has("action") || v.stopAt("action") != nil {
			return message.KindSystem
		}
		return message.KindUnknown
	}
	switch mediaOf(v, c) {
	case "empty":
		return message.KindText
	case "media":
		return message.KindMedia
	}
	return message.KindUnknown
}

// readSideBlobs states custom_params and replies_data (spec 8.5, F19): never decoded, their length and SHA-256,
// and replies_data's constructor (at least 4 bytes); a cell of another class or an unreadable one is listed in
// unknown_columns, a NULL states nothing.
func (r *msgRow) readSideBlobs(row parse.Row, c rowrun.Cols) {
	for _, name := range []string{"custom_params", "replies_data"} {
		col := c.At(name)
		if col < 0 {
			continue
		}
		b, k := blobCell(row, col)
		switch k {
		case rowrun.CellValue:
			r.raw[name+"_len"], r.raw[name+"_sha256"] = int64(len(b)), sha256Hex(b)
			if name == "replies_data" && len(b) >= 4 {
				r.raw[name+"_constructor"] = hexID(binary.LittleEndian.Uint32(b))
			}
		case rowrun.CellOther:
			r.unknown[r.unknownName(name)] = unknownTypeMismatch
		case rowrun.CellUnknown:
			r.unknown[r.unknownName(name)] = rowrun.StateOf(row, col).String()
		}
	}
}

// readReplyData states the replydata cell (spec 8.5): a blob is the reply copy (replyCopy); a cell of another class
// or an unreadable one is listed in unknown_columns; a NULL states nothing.
func (r *msgRow) readReplyData(row parse.Row, c rowrun.Cols, tab tl.Table, bud tl.Budget) {
	col := c.At(colReplyData)
	b, k := blobCell(row, col)
	switch k {
	case rowrun.CellValue:
		replyCopy(b, r.raw, tab, bud)
	case rowrun.CellOther:
		r.unknown[r.unknownName(colReplyData)] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[r.unknownName(colReplyData)] = rowrun.StateOf(row, col).String()
	}
}

// blobCell reads a BLOB cell: CellValue with the bytes, CellNull for NULL, CellOther for a value of another class
// (a stored NaN included), CellUnknown for an unreadable cell.
func blobCell(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	switch {
	case col < 0:
		return nil, rowrun.CellMissing
	case nanCell(row, col):
		return nil, rowrun.CellOther
	case !rowrun.StateOf(row, col).Known():
		return nil, rowrun.CellUnknown
	case row.IsNull(col):
		return nil, rowrun.CellNull
	}
	if b, ok := row.Blob(col); ok {
		return b, rowrun.CellValue
	}
	return nil, rowrun.CellOther
}

// budgetOf is the job budget a decode charges (nil when the context has no input: every decode then states budget).
func budgetOf(m parse.MapContext) tl.Budget {
	if in := m.Input(); in != nil && in.Budget != nil {
		return in.Budget
	}
	return nil
}

// mapMessage turns one row of a message table into its message record (spec 8.3-8.5): the plain columns as stored, the
// data blob with its state, direction, kind, status, read, guid, the text as Body and the date as the record time (a
// phone-call action is one call and no message, callOf); the dialog (resolveDialog: class, thread and joins), the
// participants (participantsOf) and the Summary (messageSummary) of spec 8.6 and 9. Every decode, the joined chats
// blob's included, charges the job budget and is freed before the row's mapping returns (F8).
func mapMessage(m parse.MapContext, row parse.Row, e *env, tab tl.Table, table string, c rowrun.Cols) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	r := &msgRow{table: table, raw: map[string]any{"source_table": table}, unknown: map[string]any{}, cells: map[string]intCell{}}
	raw := r.raw
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl
	}
	r.readPlain(row, c)
	if e != nil && e.Account != valueUnknown {
		raw["account"] = e.Account
	}
	tm, date, dateOK := r.readDate(row, c)
	msgID, msgIDOK := r.readPre84()

	var v view
	state := ""
	bud := budgetOf(m)
	b, k := blobCell(row, c.At(colData))
	switch k {
	case rowrun.CellValue:
		v, state = decodeBlob(raw, colData, b, typeMessage, tab, bud)
		defer func() {
			if bud != nil {
				bud.Free(v.r.Charged())
			}
		}()
	case rowrun.CellNull:
		raw["data_state"], state = stateNull, stateNull
	case rowrun.CellOther:
		r.unknown[r.unknownName(colData)] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[r.unknownName(colData)] = rowrun.StateOf(row, c.At(colData)).String()
	}
	top, hasTop := topOf(v, tab, typeMessage, state)
	var atts []message.Attachment
	if hasTop {
		readBlobRaw(raw, v, top, msgID, msgIDOK, date, dateOK)
		peerRaw(v, raw)
		fwdRaw(v, raw)
		atts = mediaSummary(v, tab, top, raw)
		readTrailer(b, v, top, raw)
	}

	out, outOK := r.cell("out")
	dir := directionOf(raw, out, outOK, v, top, hasTop)
	send, sendOK := r.cell("send_state")
	rs, rsOK := r.cell("read_state")
	msg := message.Message{
		Channel: channelTelegram, Direction: dir, Kind: kindOf(v, top, hasTop, state),
		Status: statusOf(raw, dir, send, sendOK), Read: readOf(raw, dir, rs, rsOK),
		Attachments: atts,
	}

	d := dialog{class: classUnknown}
	if uid, ok := r.cell("uid"); ok {
		var th *message.Thread
		if d, th, err = resolveDialog(m, e, r, v, hasTop, tab, bud, uid); err != nil {
			return nil, err
		}
		msg.Thread = th
	} else {
		raw["thread_state"] = stateUndecided
	}
	raw["dialog_class"] = string(d.class)
	msg.Participants, msg.ParticipantsUnknown = participantsOf(v, hasTop, state, dir, d, raw)

	r.readSideBlobs(row, c)
	r.readReplyData(row, c, tab, bud)
	if e == nil && putReinserts {
		// the typed pass (ruling T2): the recovered row's own key against the live rows of its own table, never a join
		mid, midOK := r.cell("mid")
		uid, uidOK := r.cell("uid")
		// messages is keyed by mid alone (T9): its uid need not be read
		raw["live_key_state"] = liveKeyState(m, table, mid, uid, midOK && (uidOK || table == tableMessages))
	}

	body := ""
	if hasTop {
		if text, ok := v.Str("message"); ok {
			cl := common.CleanText(text, maxBody)
			body = cl.Text
			if body != "" {
				msg.TextSource = message.TextSourceText
			}
			setBodyFields(&msg, cl)
		}
	}
	if len(r.unknown) > 0 {
		raw["unknown_columns"] = r.unknown
	}
	if hasTop && isPhoneCall(v, msg.Kind) {
		// spec 8.7, Q6: one call and no message for a phone-call action (a guid is a message's)
		uid, uidOK := r.cell("uid")
		c, end := callOf(v, dir, d, uid, uidOK, tm, raw)
		return []records.Record{{
			Type: call.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
			Range: &place.Range, Time: tm, TimeEnd: end,
			Summary: call.Summary(c.Direction, c.Outcome, c.Address),
			Payload: c.Payload(),
		}}, nil
	}
	if mid, ok := r.cell("mid"); ok {
		msg.GUID = strconv.FormatInt(mid, 10)
	} else {
		raw["guid_state"] = stateUndecided
	}
	msg.Raw = raw
	rec := records.Record{
		Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
		Range: &place.Range, Time: tm, Body: body,
		Summary: messageSummary(d, dir, msg.Participants, body),
		Payload: msg.Payload(),
	}
	return []records.Record{rec}, nil
}

// setBodyFields states what CleanText did to the text (C4): its flags, a cut with the total length, and the
// original bytes (base64 within 64 KiB, else SHA-256) with their length.
func setBodyFields(msg *message.Message, cl common.Cleaned) {
	if len(cl.Flags) > 0 {
		msg.BodyFlags = cl.Flags
	}
	if cl.Truncated {
		t, n := true, int64(cl.TotalBytes)
		msg.BodyTruncated, msg.BodyTotalBytes = &t, &n
	}
	if cl.RawB64 != "" || cl.RawSHA256 != "" {
		n := int64(cl.RawLen)
		msg.BodyRawB64, msg.BodyRawSHA256, msg.BodyRawLen = cl.RawB64, cl.RawSHA256, &n
	}
}
