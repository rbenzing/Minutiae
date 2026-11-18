package androidwhatsapp

import (
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// Calls (spec 6.6, 7.4, R10): one call per call_log row. The direction comes from from_me, the kind from video_call and
// the outcome from call_result, each only where two sources agree (4I-R7). The time is a millisecond value with NO
// plausibility floor, as android.calllog's ruling T7: a seconds value in the column is a 1970 date stated as stored.
// missed_call_logs is not mapped: its relation to call_log is not established, so mapping both could count one call twice.

const (
	tblCallParticipants = "call_log_participant_v2"
	serviceWhatsApp     = "whatsapp"
	maxCallDuration     = 1_000_000_000 // seconds, as android.calllog
	maxExactDuration    = 1 << 53       // every integer up to this magnitude is exactly a float64
)

func callSpecs() []parse.ColumnSpec {
	specs := []parse.ColumnSpec{{Name: "call_id", Affinity: "text"}}
	for _, n := range []string{
		"jid_row_id", "from_me", "timestamp", "video_call", "duration", "call_result", "bytes_transferred",
		"group_jid_row_id", "call_type", "call_link_row_id", "is_dnd_mode_on",
	} {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "integer"})
	}
	return specs
}

func callLogMapping() parse.TableMapping {
	return parse.TableMapping{Table: tblCallLog, Columns: callSpecs(), Map: mapCall}
}

func mapCall(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the call_log row has no rowid, so its joins cannot be made")
	}
	f := &msgFacts{Table: tblCallLog, Place: place, Rowid: id, Raw: map[string]any{}, Unknown: map[string]any{}}
	raw := f.Raw
	r := cellReader{row: row, cols: rowrun.Resolve(m, tblCallLog, specNames(callSpecs())...), raw: raw, unknown: f.Unknown}
	dp := classifyDB(m.Input().Primary.Logical)
	raw["source_table"], raw["app"], raw["app_basis"] = tblCallLog, dp.App, "path"
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl
	}

	cl := call.Call{Direction: call.DirUnknown, Outcome: call.OutcomeUnknown, Service: serviceWhatsApp}
	dir := readCode(r, "from_me", false)
	dir.Column = colCallFromMe
	if l, ok := dir.label(); ok {
		cl.Direction = l
	}
	vc := readCode(r, "video_call", false)
	vc.Column = colVideoCall
	if l, ok := vc.label(); ok {
		if l == "video" {
			cl.Kind = call.KindVideo
		} else {
			cl.Kind = call.KindVoice
		}
	}
	res := readCode(r, "call_result", false)
	res.Column = colCallResult
	label, known := res.label()
	switch {
	case known && label == "answered":
		cl.Outcome = call.OutcomeAnswered
	case known && label == "not_answered" && cl.Direction == call.DirIn:
		cl.Outcome = call.OutcomeMissed
	case known:
		// the value is known but no outcome fits: the research label is kept for the examiner (spec 7.4)
		raw["label_call_result"] = label
	}

	rec := records.Record{Type: call.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	dur, dok := r.int("duration")
	if dok {
		raw["duration"] = dur
	}
	if exact := dur >= -maxExactDuration && dur <= maxExactDuration; dok && !exact {
		f.Unknown["duration"] = "precision" // beyond 2^53 an integer is not a float64: stating its rounding would invent a value
	} else if dok && dur >= 0 {
		v := float64(dur)
		cl.DurationS = &v
	}
	if v, ok := r.int("timestamp"); ok {
		in := rowrun.Millis(v) // R10: no floor
		in.Record(raw, "timestamp")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
			if dok && dur > 0 && dur <= maxCallDuration {
				if end := in.T.Add(time.Duration(dur) * time.Second); end.Before(ts.Max()) {
					rec.TimeEnd = &records.Time{T: end}
					raw["time_end_basis"] = "start+duration"
				}
			}
		}
	}

	if err := readCallAddress(m, r, f, &cl); err != nil {
		return nil, err
	}
	if err := readCallGroup(m, r, f); err != nil {
		return nil, err
	}
	if b, ok := r.text("call_id"); ok {
		raw["call_id"] = cleanField(raw, "call_id", b)
	}
	r.rawInts("bytes_transferred", "call_type", "call_link_row_id", "is_dnd_mode_on")
	if err := readCallParticipants(m, f); err != nil {
		return nil, err
	}
	if len(f.Unknown) > 0 {
		raw["unknown_columns"] = f.Unknown
	}
	cl.Raw = raw
	rec.Summary = call.Summary(cl.Direction, cl.Outcome, cl.Address)
	rec.Payload = cl.Payload()
	return []records.Record{rec}, nil
}

// readCallAddress joins the counterparty: the jid row of jid_row_id. A LID is stated as for a message (D6).
func readCallAddress(m parse.MapContext, r cellReader, f *msgFacts, cl *call.Call) error {
	id, ok := r.int("jid_row_id")
	if !ok || id <= 0 {
		f.Raw["address_state"] = stateAbsent
		return nil
	}
	a, st, err := jidString(m, f, id, "address")
	if err != nil {
		return err
	}
	f.Chat = a
	if a.Str != "" {
		cl.Address = a.Str
	} else {
		f.Raw["address_state"] = st
		if a.Parts != nil {
			f.Raw["address_parts"] = a.Parts
		}
	}
	return lidMappings(m, f, nil)
}

// readCallGroup states the group of a group call: group_jid_row_id 0 is no group (S6, raw only), above 0 names a jid row.
func readCallGroup(m parse.MapContext, r cellReader, f *msgFacts) error {
	gid, ok := r.int("group_jid_row_id")
	if !ok || gid == 0 {
		return nil
	}
	if gid < 0 {
		f.Raw["group_jid_state"] = stateAbsent
		return nil
	}
	a, st, err := jidString(m, f, gid, "group_jid")
	if err != nil {
		return err
	}
	if a.Str != "" {
		f.Raw["group_jid"] = a.Str
	} else {
		f.Raw["group_jid_state"] = st
	}
	return nil
}

// readCallParticipants lists the call_log_participant_v2 rows of the call as [{jid, call_result}], at most maxList.
// The participant results rest on S6 alone, so they stay raw.
func readCallParticipants(m parse.MapContext, f *msgFacts) error {
	rows, st, err := matchInt(m, tblCallParticipants, "call_log_row_id", f.Rowid)
	if err != nil {
		return err
	}
	switch st {
	case jNoTable:
		return nil
	case jUndecided:
		undecidedJoin(f.Raw, tblCallParticipants)
		return nil
	}
	var list []any
	for i, prow := range rows {
		if i >= maxList {
			break
		}
		pr := cellReader{row: prow, cols: rowrun.Resolve(m, tblCallParticipants, "jid_row_id", "call_result"), raw: f.Raw, unknown: f.Unknown, prefix: tblCallParticipants + "."}
		e := map[string]any{}
		pj, ok := pr.int("jid_row_id")
		switch {
		case !ok || pj <= 0:
			e["jid_state"] = stateAbsent
		default:
			a, jst, err := jidString(m, f, pj, "participant_jid")
			if err != nil {
				return err
			}
			if a.Str != "" {
				e["jid"] = a.Str
			} else {
				e["jid_state"] = jst
			}
		}
		if v, ok := pr.int("call_result"); ok {
			e["call_result"] = v
		}
		list = append(list, e)
	}
	if len(list) > 0 {
		f.Raw["participants"] = list
	}
	if len(rows) > maxList {
		f.Raw["participants_total"], f.Raw["participants_truncated"] = int64(len(rows)), true
	}
	return nil
}
