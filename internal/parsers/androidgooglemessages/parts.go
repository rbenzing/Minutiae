package androidgooglemessages

import (
	"errors"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// partsSpecs are the columns the parts mapping reads (spec 6.4, 6.5, 8.2): the signature columns first.
func partsSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "message_id", Required: true, Affinity: "integer"},
		{Name: "text", Required: true, Affinity: "text"},
		{Name: "content_type", Required: true, Affinity: "text"},
		{Name: "uri", Required: true, Affinity: "text"},
		{Name: "width", Affinity: "integer"},
		{Name: "height", Affinity: "integer"},
		{Name: "timestamp", Affinity: "integer"},
		{Name: "conversation_id", Affinity: "integer"},
	}
}

// partsMapping accounts for every parts row of the live pass (spec 6.4, 7 R11 and R12, B6): a part whose message is in the
// decided id set is joined into that message's record (counted), a part of no decided message is a record of its own (an
// orphan) while the set is complete, and a row warning while it is not. A nil e is the typed pass (spec 8.2, 8.3): every
// recovered part is a record of its own (mapRecoveredPart), joined to a live message only by structure; the join basis
// (spec 8.3 condition 1) is decided once per run, by the first recovered part, and held in this closure (4I5-P7).
func partsMapping(e *env) parse.TableMapping {
	var basis *joinBasis
	return parse.TableMapping{
		Table: tableParts, Columns: partsSpecs(),
		Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			if e == nil {
				if basis == nil {
					b, err := joinBasisOf(m)
					if err != nil { // a budget, context or I/O failure: not decided, so asked again by the next row
						return nil, err
					}
					basis = &b
				}
				return mapRecoveredPart(m, row, basis)
			}
			return mapPart(m, row, e)
		},
	}
}

// mapPart is the live parts mapping of one row. It never reads the messages table: the decided id set stands for it.
func mapPart(m parse.MapContext, row parse.Row, e *env) ([]records.Record, error) {
	c := rowrun.Resolve(m, tableParts, specNames(partsSpecs())...)
	mid, mk := intCellOf(row, c.At("message_id"))
	if mk == rowrun.CellValue && e.IDs.has(mid) {
		e.Counts.PartsJoined++
		return nil, nil
	}
	if !e.IDs.complete {
		return nil, rowrun.Unmappable("part's message is undecided: the message-id set is incomplete (%s)", e.IDs.cause)
	}
	rec, err := orphanPart(m, row, c, mid, mk)
	if err != nil {
		return nil, err
	}
	e.Counts.Orphans++
	return []records.Record{rec}, nil
}

// partCells are the cells of one parts row as the body and the attachments use them.
type partCells struct {
	Text    []byte
	HasText bool
	HasURI  bool
	Att     message.Attachment
	Apart   map[string]any // the attachment_parts entry: part_id, width, height as stored
}

// readPart reads the text and the media of a part (spec 6.4, 6.5). prefix names the unknown_columns keys: "parts." inside a
// message, "" for a part that is a record of its own. A cell that cannot be read is listed and the part is neither text nor
// media by it. The attachment carries the cleaned content type and uri only: never a name, a size or an artifact (they are
// never looked up).
func readPart(row parse.Row, c rowrun.Cols, raw, unknown map[string]any, prefix string) partCells {
	var p partCells
	tb, tk := textCellOf(row, c.At("text"))
	rowrun.Flag(unknown, row, prefix+"text", c.At("text"), tk)
	if tk == rowrun.CellValue {
		p.Text, p.HasText = tb, true
	}
	ub, uk := textCellOf(row, c.At("uri"))
	rowrun.Flag(unknown, row, prefix+"uri", c.At("uri"), uk)
	if uk != rowrun.CellValue {
		return p
	}
	p.HasURI = true
	cb, ck := textCellOf(row, c.At("content_type"))
	rowrun.Flag(unknown, row, prefix+"content_type", c.At("content_type"), ck)
	if ck == rowrun.CellValue {
		p.Att.Mime = cleanField(raw, "attachments.mime", cb, maxShort)
	}
	p.Att.SourceRef = cleanField(raw, "attachments.source_ref", ub, maxShort)
	p.Apart = map[string]any{}
	if id, ok := row.Rowid(); ok {
		p.Apart["part_id"] = id
	}
	for _, col := range []string{"width", "height"} {
		v, k := intCellOf(row, c.At(col))
		rowrun.Flag(unknown, row, prefix+col, c.At(col), k)
		if k == rowrun.CellValue {
			p.Apart[col] = v
		}
	}
	return p
}

// partsUndecided says the parts of a message cannot be decided: the index lost rows or hit its entry cap, or the table is
// damaged. A budget, context or I/O failure is not that and fails the job.
func partsUndecided(err error) bool {
	return joinUndecided(err) || errors.Is(err, sqlitedb.ErrIndexLimit)
}

// bodyOf states the parts of one message (spec 6.4, 6.5, B6): its parts in id order (at most maxList), the Body from the
// text parts joined with a newline, the attachments of the media parts, the kind. No part is no Body (raw.parts 0, body_state
// no-parts). A parts join that cannot be decided is no Body, body_state undecided and parts in join_undecided, never
// no-parts.
func bodyOf(m parse.MapContext, msgID int64, raw, unknown map[string]any, msg *message.Message, rec *records.Record) error {
	rows, short, err := rowrun.Match(m, tableParts, "message_id", msgID)
	switch {
	case err != nil && !partsUndecided(err):
		return err
	case err != nil || short:
		raw["body_state"] = stateUndecided
		undecidedJoin(raw, tableParts)
		return nil
	case len(rows) == 0:
		raw["parts"] = int64(0)
		raw["body_state"] = "no-parts"
		return nil
	}
	if total := len(rows); total > maxList {
		raw["parts_total"], raw["parts_truncated"] = int64(total), true
		rows = rows[:maxList]
	}
	raw["parts"] = int64(len(rows))
	c := rowrun.Resolve(m, tableParts, specNames(partsSpecs())...)
	var texts [][]byte
	var apart []any
	var both, empty int64
	for _, row := range rows {
		p := readPart(row, c, raw, unknown, "parts.")
		if p.HasText {
			texts = append(texts, p.Text)
		}
		if p.HasURI {
			msg.Attachments = append(msg.Attachments, p.Att)
			apart = append(apart, p.Apart)
		}
		switch {
		case p.HasText && p.HasURI:
			both++
		case !p.HasText && !p.HasURI:
			empty++
		}
	}
	if both > 0 {
		raw["parts_text_and_uri"] = both
	}
	if empty > 0 {
		raw["parts_empty"] = empty
	}
	if len(apart) > 0 {
		raw["attachment_parts"] = apart
	}
	switch {
	case len(msg.Attachments) > 0:
		msg.Kind = message.KindMedia
	case len(texts) > 0:
		msg.Kind = message.KindText
	}
	if len(texts) > 0 {
		joined, total := joinText(texts)
		cl := cleanBody(joined, total)
		rec.Body = cl.Text
		msg.TextSource = message.TextSourcePart
		setBody(msg, cl)
	}
	return nil
}

// orphanPart is the record of a part whose message is not among the decided ones while the set is complete (R11, spec 8.2
// shape): its own columns only, channel and direction unknown, no participants, the thread by the part's conversation_id.
func orphanPart(m parse.MapContext, row parse.Row, c rowrun.Cols, mid int64, mk rowrun.CellKind) (records.Record, error) {
	p, err := ownPart(m, row, c, mid, mk)
	if err != nil {
		return records.Record{}, err
	}
	p.raw["part_state"] = "orphan"
	return p.finish(row, ""), nil
}

// partRecord is a part that is a record of its own while it is being built: the record, its message and their raw and
// unknown_columns maps.
type partRecord struct {
	rec          records.Record
	msg          message.Message
	raw, unknown map[string]any
}

// ownPart reads a part that is a record of its own (an orphan, a recovered part; spec 8.2): its place, the app, part_rowid,
// part_message_id, timestamp, conversation_id and the thread by it, and its Body or attachment. The channel and the direction
// are unknown and the participants unknown until the caller states otherwise.
func ownPart(m parse.MapContext, row parse.Row, c rowrun.Cols, mid int64, mk rowrun.CellKind) (partRecord, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return partRecord{}, err
	}
	raw := map[string]any{}
	unknown := map[string]any{}
	raw["app"], raw["app_basis"] = appOf(m)
	msg := message.Message{Channel: channelUnknown, Direction: message.DirUnknown, Kind: message.KindUnknown, ParticipantsUnknown: true, Raw: raw}
	if id, ok := row.Rowid(); ok {
		raw["part_rowid"] = id
	}
	switch mk {
	case rowrun.CellValue:
		raw["part_message_id"] = mid
	case rowrun.CellNull:
		raw["part_message_id_state"] = stateNull
	default:
		rowrun.Flag(unknown, row, "message_id", c.At("message_id"), mk)
	}
	if v, k := intCellOf(row, c.At("timestamp")); k == rowrun.CellValue {
		rowrun.MillisFrom1973(v).Record(raw, "timestamp")
	} else {
		rowrun.Flag(unknown, row, "timestamp", c.At("timestamp"), k)
	}
	if v, k := intCellOf(row, c.At("conversation_id")); k == rowrun.CellValue {
		raw["conversation_id"] = v
		msg.Thread = &message.Thread{ID: strconv.FormatInt(v, 10)}
	} else {
		rowrun.Flag(unknown, row, "conversation_id", c.At("conversation_id"), k)
	}
	p := readPart(row, c, raw, unknown, "")
	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if p.HasText {
		cl := common.CleanText(p.Text, maxBody)
		if len(cl.Flags) > 0 {
			markText(raw, "body")
		}
		rec.Body = cl.Text
		msg.TextSource = message.TextSourcePart
		setBody(&msg, cl)
	}
	if p.HasURI {
		msg.Attachments = []message.Attachment{p.Att}
		raw["attachment_parts"] = []any{p.Apart}
		msg.Kind = message.KindMedia
	} else if p.HasText {
		msg.Kind = message.KindText
	}
	switch {
	case p.HasText && p.HasURI:
		raw["parts_text_and_uri"] = int64(1)
	case !p.HasText && !p.HasURI:
		raw["parts_empty"] = int64(1)
	}
	return partRecord{rec: rec, msg: msg, raw: raw, unknown: unknown}, nil
}

// finish states the unread cells and the decoder's flags of the row and makes the summary and the payload.
func (p *partRecord) finish(row parse.Row, counterparty string) records.Record {
	if len(p.unknown) > 0 {
		p.raw["unknown_columns"] = p.unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		p.raw["row_flags"] = f
	}
	p.rec.Summary = message.Summary(p.msg.Channel, p.msg.Direction, counterparty, p.rec.Body)
	p.rec.Payload = p.msg.Payload()
	return p.rec
}
