package androidmms

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// smsSpecs is the mapping's column list in the order the oracle uses; Required
// columns are the ones Probe insists on.
func smsSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "address", Required: true, Affinity: "text"}, {Name: "date", Required: true, Affinity: "integer"},
		{Name: "type", Required: true, Affinity: "integer"}, {Name: "body", Required: true, Affinity: "text"},
		{Name: "thread_id", Affinity: "integer"}, {Name: "date_sent", Affinity: "integer"}, {Name: "read", Affinity: "integer"},
		{Name: "status", Affinity: "integer"}, {Name: "subject", Affinity: "text"}, {Name: "protocol", Affinity: "integer"},
		{Name: "service_center", Affinity: "text"}, {Name: "locked", Affinity: "integer"}, {Name: "sub_id", Affinity: "integer"},
		{Name: "error_code", Affinity: "integer"}, {Name: "creator", Affinity: "text"}, {Name: "seen", Affinity: "integer"},
	}
}

func smsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "sms", Columns: smsSpecs(), Map: mapSMS}
}

// smsType maps sms.type (spec 9.1); the raw value is always kept. An unrecognised
// or missing type is unknown, never a guessed direction.
func smsType(v int64, known bool) (direction, status string) {
	if known {
		switch v {
		case 1:
			return message.DirIn, message.StatusReceived
		case 2:
			return message.DirOut, message.StatusSent
		case 3:
			return message.DirOut, message.StatusDraft
		case 4, 6:
			return message.DirOut, message.StatusQueued
		case 5:
			return message.DirOut, message.StatusFailed
		}
	}
	return message.DirUnknown, message.StatusUnknown
}

func roleFor(direction string) string {
	switch direction {
	case message.DirIn:
		return message.RoleFrom
	case message.DirOut:
		return message.RoleTo
	}
	return message.RoleMember
}

func mapSMS(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "sms", specNames(smsSpecs())...)
	raw := map[string]any{}
	unknown := map[string]any{}
	msg := message.Message{Channel: message.ChannelSMS, Kind: message.KindText, Raw: raw}

	typ, tk := rowrun.IntCell(row, c.At("type"))
	rowrun.Flag(unknown, row, "type", c.At("type"), tk)
	msg.Direction, msg.Status = smsType(typ, tk == rowrun.CellValue)

	addr, ak := rowrun.TextCell(row, c.At("address"))
	rowrun.Flag(unknown, row, "address", c.At("address"), ak)
	counterparty := ""
	if p, ok := participant(addr, ak, roleFor(msg.Direction), raw, "address"); ok {
		msg.Participants = []message.Participant{p}
		counterparty = p.Address
	} else {
		msg.ParticipantsUnknown = true
	}

	tid, tidk := rowrun.IntCell(row, c.At("thread_id"))
	rowrun.Flag(unknown, row, "thread_id", c.At("thread_id"), tidk)
	if tidk == rowrun.CellValue {
		raw["thread_id"] = tid
		if msg.Thread, err = threadFor(m, tid, raw); err != nil {
			return nil, err
		}
	}

	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	date, dk := rowrun.IntCell(row, c.At("date"))
	rowrun.Flag(unknown, row, "date", c.At("date"), dk)
	if dk == rowrun.CellValue {
		in := rowrun.MillisFrom1973(date)
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
		in := rowrun.MillisFrom1973(sent)
		in.Record(raw, "date_sent")
		if in.Valid() && msg.Direction == message.DirIn {
			rec.Times = append(rec.Times, records.NamedTime{Kind: "sent", Time: records.Time{T: in.T}})
		}
	}

	body, bk := rowrun.TextCell(row, c.At("body"))
	rowrun.Flag(unknown, row, "body", c.At("body"), bk)
	if bk == rowrun.CellValue {
		cl := common.CleanText(body, maxBody)
		rec.Body = cl.Text
		msg.TextSource = message.TextSourceText
		setBody(&msg, cl)
	}
	sub, subk := rowrun.TextCell(row, c.At("subject"))
	rowrun.Flag(unknown, row, "subject", c.At("subject"), subk)
	if subk == rowrun.CellValue {
		msg.Subject = cleanField(raw, "subject", sub)
	}
	if rd, rk := rowrun.IntCell(row, c.At("read")); rk == rowrun.CellValue && (rd == 0 || rd == 1) {
		b := rd == 1
		msg.Read = &b
	}
	if sid, sidk := rowrun.IntCell(row, c.At("sub_id")); sidk == rowrun.CellValue && sid >= 0 {
		msg.Subscription = &sid
	}
	for _, name := range []string{"type", "status", "protocol", "read", "seen", "locked", "error_code", "sub_id"} {
		v, k := rowrun.IntCell(row, c.At(name))
		if k == rowrun.CellValue {
			raw[name] = v
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}
	for _, name := range []string{"service_center", "creator"} {
		v, k := rowrun.TextCell(row, c.At(name))
		if k == rowrun.CellValue {
			raw[name] = cleanField(raw, name, v)
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Summary = message.Summary(message.ChannelSMS, msg.Direction, counterparty, rec.Body)
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}
