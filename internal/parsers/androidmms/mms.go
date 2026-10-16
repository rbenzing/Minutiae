package androidmms

import (
	"slices"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func pduSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "date", Required: true, Affinity: "integer"}, {Name: "msg_box", Required: true, Affinity: "integer"},
		{Name: "thread_id", Affinity: "integer"}, {Name: "date_sent", Affinity: "integer"}, {Name: "read", Affinity: "integer"},
		{Name: "m_id", Affinity: "text"}, {Name: "sub", Affinity: "text"}, {Name: "m_type", Affinity: "integer"},
		{Name: "m_size", Affinity: "integer"}, {Name: "ct_t", Affinity: "text"}, {Name: "locked", Affinity: "integer"},
		{Name: "seen", Affinity: "integer"}, {Name: "sub_id", Affinity: "integer"}, {Name: "text_only", Affinity: "integer"},
	}
}

func pduMapping() parse.TableMapping {
	return parse.TableMapping{Table: "pdu", Columns: pduSpecs(), Map: mapPDU}
}

func pduBox(v int64, known bool) (direction, status string) {
	if known {
		switch v {
		case 1:
			return message.DirIn, message.StatusReceived
		case 2:
			return message.DirOut, message.StatusSent
		case 3:
			return message.DirOut, message.StatusDraft
		case 4:
			return message.DirOut, message.StatusQueued
		}
	}
	return message.DirUnknown, message.StatusUnknown
}

func addrRole(v int64, known bool) string {
	if known {
		switch v {
		case 137:
			return message.RoleFrom
		case 151:
			return message.RoleTo
		case 129:
			return message.RoleBCC
		case 130:
			return message.RoleCC
		}
	}
	return message.RoleMember
}

func mapPDU(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the pdu row has no rowid, so its addresses and parts cannot be joined")
	}
	c := rowrun.Resolve(m, "pdu", specNames(pduSpecs())...)
	raw := map[string]any{}
	unknown := map[string]any{}
	msg := message.Message{Channel: message.ChannelMMS, Raw: raw}

	box, bk := rowrun.IntCell(row, c.At("msg_box"))
	rowrun.Flag(unknown, row, "msg_box", c.At("msg_box"), bk)
	msg.Direction, msg.Status = pduBox(box, bk == rowrun.CellValue)
	mtype, mk := rowrun.IntCell(row, c.At("m_type"))
	rowrun.Flag(unknown, row, "m_type", c.At("m_type"), mk)
	var undecided []any

	// participants: the addr rows of this message
	arows, au, err := rowrun.Match(m, "addr", "msg_id", id)
	if err != nil {
		return nil, err
	}
	if au {
		undecided = append(undecided, "addr")
	}
	if !au || len(arows) > 0 {
		raw["addr_rows"] = int64(len(arows))
	}
	ac := rowrun.Resolve(m, "addr", "address", "type")
	for _, a := range arows {
		ty, tk := rowrun.IntCell(a, ac.At("type"))
		ab, abk := rowrun.TextCell(a, ac.At("address"))
		if p, ok := participant(ab, abk, addrRole(ty, tk == rowrun.CellValue), raw, "addr"); ok {
			msg.Participants = append(msg.Participants, p)
		}
	}
	if len(msg.Participants) > maxList {
		raw["participants_total"], raw["participants_truncated"] = int64(len(msg.Participants)), true
		msg.Participants = msg.Participants[:maxList]
	}
	msg.ParticipantsUnknown = len(msg.Participants) == 0

	// parts: text parts form the body, smil parts are counted, the rest are attachments
	prows, pu, err := rowrun.Match(m, "part", "mid", id)
	if err != nil {
		return nil, err
	}
	if pu {
		undecided = append(undecided, "part")
	}
	pc := rowrun.Resolve(m, "part", "seq", "ct", "name", "cl", "_data", "text")
	seq := func(r parse.Row) int64 {
		if v, k := rowrun.IntCell(r, pc.At("seq")); k == rowrun.CellValue {
			return v
		}
		return 0
	}
	slices.SortStableFunc(prows, func(a, b parse.Row) int {
		x, y := seq(a), seq(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	if len(prows) > 0 || !pu {
		raw["parts"] = int64(len(prows))
	}
	var texts [][]byte
	var atts []parse.Row
	var smil, notext int64
	for _, q := range prows {
		ctb, ck := rowrun.TextCell(q, pc.At("ct"))
		ct := ""
		if ck == rowrun.CellValue {
			ct = asciiLower(ctb)
		}
		switch {
		case ck == rowrun.CellValue && ct == "text/plain":
			if tb, tk := rowrun.TextCell(q, pc.At("text")); tk == rowrun.CellValue {
				texts = append(texts, tb)
			} else {
				notext++
			}
		case ck == rowrun.CellValue && ct == "application/smil":
			smil++
		default:
			atts = append(atts, q)
		}
	}
	if smil > 0 {
		raw["smil_parts"] = smil
	}
	if notext > 0 {
		raw["text_parts_without_text"] = notext
	}
	body := ""
	if len(texts) > 0 {
		joined, total := joinText(texts)
		cl := common.CleanText(joined, maxBody)
		if total > int64(len(joined)) {
			cl.Truncated, cl.TotalBytes = true, int(total)
			if !slices.Contains(cl.Flags, common.FlagTruncated) {
				cl.Flags = append(cl.Flags, common.FlagTruncated)
			}
		}
		body = cl.Text
		msg.TextSource = message.TextSourcePart
		setBody(&msg, cl)
	}
	var resolved, unresolved, ambiguous, otherSource int64
	for i, q := range atts {
		if i >= maxList {
			break
		}
		var a message.Attachment
		if nb, k := rowrun.TextCell(q, pc.At("name")); k == rowrun.CellValue {
			a.Name = cleanField(raw, "attachments.name", nb)
		}
		if a.Name == "" {
			if cb, k := rowrun.TextCell(q, pc.At("cl")); k == rowrun.CellValue {
				a.Name = cleanField(raw, "attachments.name", cb)
			}
		}
		if mb, k := rowrun.TextCell(q, pc.At("ct")); k == rowrun.CellValue {
			a.Mime = cleanField(raw, "attachments.mime", mb)
		}
		if db, k := rowrun.TextCell(q, pc.At("_data")); k == rowrun.CellValue {
			if ref := cleanField(raw, "attachments.source_ref", db); ref != "" {
				a.SourceRef = ref
				art, st := resolveAttachment(m.Input(), ref)
				switch st {
				case lookupFound:
					size := art.Size
					a.ArtifactID, a.ArtifactSHA256, a.Size = art.ID, art.SHA256, &size
					resolved++
				case lookupAmbiguous:
					ambiguous++
				case lookupOtherSource:
					otherSource++
				default:
					unresolved++
				}
			}
		}
		msg.Attachments = append(msg.Attachments, a)
	}
	if len(msg.Attachments) > 0 {
		raw["attachments_resolved"], raw["attachments_unresolved"] = resolved, unresolved
	}
	if ambiguous > 0 {
		raw["attachments_ambiguous"] = ambiguous
	}
	if otherSource > 0 {
		raw["attachments_other_source"] = otherSource
	}
	if len(atts) > maxList {
		raw["attachments_total"], raw["attachments_truncated"] = int64(len(atts)), true
	}
	switch {
	case mk == rowrun.CellValue && mtype != 128 && mtype != 132:
		msg.Kind = message.KindSystem
	case len(atts) > 0:
		msg.Kind = message.KindMedia
	case len(texts) > 0:
		msg.Kind = message.KindText
	default:
		msg.Kind = message.KindUnknown
	}

	tid, tidk := rowrun.IntCell(row, c.At("thread_id"))
	rowrun.Flag(unknown, row, "thread_id", c.At("thread_id"), tidk)
	if tidk == rowrun.CellValue {
		raw["thread_id"] = tid
		if msg.Thread, err = threadFor(m, tid, raw); err != nil {
			return nil, err
		}
	}

	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range, Body: body}
	date, dk := rowrun.IntCell(row, c.At("date"))
	rowrun.Flag(unknown, row, "date", c.At("date"), dk)
	if dk == rowrun.CellValue {
		in := rowrun.Seconds(date)
		in.Record(raw, "date")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
			switch msg.Direction {
			case message.DirIn:
				rec.Times = append(rec.Times, records.NamedTime{Kind: "received", Time: records.Time{T: in.T}})
			case message.DirOut:
				rec.Times = append(rec.Times, records.NamedTime{Kind: "sent", Time: records.Time{T: in.T}})
			}
		}
	}
	sent, sk := rowrun.IntCell(row, c.At("date_sent"))
	rowrun.Flag(unknown, row, "date_sent", c.At("date_sent"), sk)
	if sk == rowrun.CellValue {
		in := rowrun.Seconds(sent)
		in.Record(raw, "date_sent")
		if in.Valid() && msg.Direction == message.DirIn {
			rec.Times = append(rec.Times, records.NamedTime{Kind: "sent", Time: records.Time{T: in.T}})
		}
	}
	if sb, k := rowrun.TextCell(row, c.At("sub")); k == rowrun.CellValue {
		msg.Subject = cleanField(raw, "subject", sb)
	} else {
		rowrun.Flag(unknown, row, "sub", c.At("sub"), k)
	}
	if gb, k := rowrun.TextCell(row, c.At("m_id")); k == rowrun.CellValue {
		msg.GUID = cleanField(raw, "guid", gb)
	} else {
		rowrun.Flag(unknown, row, "m_id", c.At("m_id"), k)
	}
	if rd, k := rowrun.IntCell(row, c.At("read")); k == rowrun.CellValue && (rd == 0 || rd == 1) {
		b := rd == 1
		msg.Read = &b
	}
	if sid, k := rowrun.IntCell(row, c.At("sub_id")); k == rowrun.CellValue && sid >= 0 {
		msg.Subscription = &sid
	}
	for _, name := range []string{"msg_box", "m_type", "read", "seen", "locked", "text_only", "m_size", "sub_id"} {
		v, k := rowrun.IntCell(row, c.At(name))
		if k == rowrun.CellValue {
			raw[name] = v
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}
	if tb, k := rowrun.TextCell(row, c.At("ct_t")); k == rowrun.CellValue {
		raw["ct_t"] = cleanField(raw, "ct_t", tb)
	} else {
		rowrun.Flag(unknown, row, "ct_t", c.At("ct_t"), k)
	}
	if len(undecided) > 0 {
		raw["join_undecided"] = undecided
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	counterparty := ""
	want := message.RoleTo
	if msg.Direction == message.DirIn {
		want = message.RoleFrom
	}
	for _, p := range msg.Participants {
		if p.Role == want {
			counterparty = p.Address
			break
		}
	}
	rec.Summary = message.Summary(message.ChannelMMS, msg.Direction, counterparty, body)
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}
