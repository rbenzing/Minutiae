package androidgooglemessages

import (
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// idSet (B6, Task 6) and memberCache (B3, Task 5) are the per-job states the later tasks fill; they are declared here so
// env has its final shape (4I5-P6).
type (
	idSet       struct{}
	memberCache struct{}
)

// env is the per-job state of the mappings (4I5-P6): the join guard, the decided message ids, the member cache and the
// counts. A nil *env is the typed pass (Mappings), which joins nothing. IDs and Members are nil until Tasks 6 and 5.
type env struct {
	Guard   *joinGuard
	IDs     *idSet
	Members *memberCache
	Counts  *counts
}

// messagesSpecs are the columns the messages mapping reads, in the order of the oracle (the signature columns first, which
// Probe insists on).
func messagesSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "conversation_id", Required: true, Affinity: "integer"},
		{Name: "sender_id", Required: true, Affinity: "integer"},
		{Name: "received_timestamp", Required: true, Affinity: "integer"},
		{Name: "message_protocol", Required: true, Affinity: "integer"},
		{Name: "message_status", Required: true, Affinity: "integer"},
		{Name: "sent_timestamp", Affinity: "integer"},
		{Name: "seen", Affinity: "integer"},
		{Name: "read", Affinity: "integer"},
		{Name: "sms_message_uri", Affinity: "text"},
		{Name: "sms_priority", Affinity: "integer"},
		{Name: "sms_message_size", Affinity: "integer"},
		{Name: "mms_subject", Affinity: "text"},
		{Name: "mms_transaction_id", Affinity: "text"},
		{Name: "mms_content_location", Affinity: "text"},
		{Name: "mms_expiry", Affinity: "integer"},
		{Name: "raw_status", Affinity: "integer"},
		{Name: "self_id", Affinity: "integer"},
		{Name: "retry_start_timestamp", Affinity: "integer"},
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// messagesMapping maps every messages row to one message record (spec 6, C2); a nil e is the typed pass.
func messagesMapping(e *env) parse.TableMapping {
	return parse.TableMapping{
		Table: tableMessages, Columns: messagesSpecs(),
		Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) { return mapMessage(e, m, row) },
	}
}

// storedInts are the integer columns kept in raw as stored (spec 6.8) beside the ones with a rule of their own.
var storedInts = []string{"seen", "sms_priority", "sms_message_size", "raw_status"}

// mapMessage is the mapping of one messages row (spec 6.1, 6.2, 6.6 to 6.8): the channel, the direction and status of the
// cited codes, the read state, the times, the stored scalars and the telephony link. The participants, the thread, the
// body and the attachments follow in Tasks 5 and 6: until then the record states participants_unknown, the kind unknown and
// the thread by its id alone.
func mapMessage(_ *env, m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, tableMessages, specNames(messagesSpecs())...)
	raw := map[string]any{}
	unknown := map[string]any{}
	raw["app"], raw["app_basis"] = appOf(m)

	msg := message.Message{Kind: message.KindUnknown, ParticipantsUnknown: true, Raw: raw}
	msg.Channel = channelOf(row, c.At("message_protocol"), raw, unknown)
	msg.Direction, msg.Status = statusOf(row, c.At("message_status"), raw, unknown)
	if cid, k := intCellOf(row, c.At("conversation_id")); k == rowrun.CellValue {
		msg.Thread = &message.Thread{ID: strconv.FormatInt(cid, 10)}
	}

	rd, rk := intCellOf(row, c.At("read"))
	rowrun.Flag(unknown, row, "read", c.At("read"), rk)
	if rk == rowrun.CellValue {
		raw["read"] = rd
		if rd == 0 || rd == 1 {
			b := rd == 1
			msg.Read = &b
		}
	}
	for _, name := range storedInts {
		v, k := intCellOf(row, c.At(name))
		if k == rowrun.CellValue {
			raw[name] = v
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}
	if v, k := intCellOf(row, c.At("retry_start_timestamp")); k == rowrun.CellValue {
		raw["retry_start_timestamp"] = v
		raw["retry_start_timestamp_unit"] = "unestablished"
	} else {
		rowrun.Flag(unknown, row, "retry_start_timestamp", c.At("retry_start_timestamp"), k)
	}
	if v, k := intCellOf(row, c.At("mms_expiry")); k == rowrun.CellValue {
		rowrun.MillisFrom1973(v).Record(raw, "mms_expiry")
	} else {
		rowrun.Flag(unknown, row, "mms_expiry", c.At("mms_expiry"), k)
	}
	for _, name := range []string{"mms_transaction_id", "mms_content_location"} {
		v, k := textCellOf(row, c.At(name))
		if k == rowrun.CellValue {
			raw[name] = cleanField(raw, name, v, maxShort)
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}
	smsLink(raw, unknown, row, c.At("sms_message_uri"))

	rec := records.Record{Type: message.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	rec.Time, rec.Times = timesOf(row, c, raw, unknown, msg.Direction)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Summary = message.Summary(msg.Channel, msg.Direction, "", "")
	rec.Payload = msg.Payload()
	return []records.Record{rec}, nil
}

// appOf is raw.app and raw.app_basis (spec 5.2): the app from the path of the database, the basis path; away from the
// app path unknown with the basis none.
func appOf(m parse.MapContext) (app, basis string) {
	in := m.Input()
	if in == nil {
		return appUnknown, "none"
	}
	if pl, a := classifyPlace(in.Primary.Logical); pl == placeApp {
		return a, "path"
	}
	return appUnknown, "none"
}
