package androidtelegram_test

import (
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/records"
)

// The wire facts of a phone-call service message, written from constructors.tsv (TL_messageService 0x7a800e0a, the
// phone-call action 0x80e11a7f and the discard reasons), independently of the parser's table.
const (
	idTLService    = 0x7a800e0a
	idCallAction   = 0x80e11a7f
	idReasonMissed = 0x85e42301
	idReasonBusy   = 0xfaf7e8c9
	idReasonHangup = 0x57adc690
	bitCallReason  = 1 << 0
	bitCallTime    = 1 << 1
	bitCallVideo   = 1 << 2
)

// tlCall is a phone-call action: reason (an encoded discard reason, ID first) and duration are set when given.
type tlCall struct {
	id       int64
	reason   []byte
	duration *int32
	video    bool
}

func (c tlCall) bytes() []byte {
	var flags uint32
	if c.reason != nil {
		flags |= bitCallReason
	}
	if c.duration != nil {
		flags |= bitCallTime
	}
	if c.video {
		flags |= bitCallVideo
	}
	b := append(le32(idCallAction), le32(flags)...)
	b = append(b, le64(c.id)...)
	b = append(b, c.reason...)
	if c.duration != nil {
		b = append(b, le32(uint32(*c.duration))...)
	}
	return b
}

// tlService is a TL_messageService: flags (from_id gated by its bit), id, [from_id], peer_id (the encoded peer), date,
// then the action bytes as given (none: a blob cut before the action).
func tlService(id int32, from int64, peer, action []byte) []byte {
	var flags uint32
	if from != 0 {
		flags |= bitFromID
	}
	b := append(le32(idTLService), le32(flags)...)
	b = append(b, le32(uint32(id))...)
	if from != 0 {
		b = append(b, peerUser(from)...)
	}
	b = append(b, peer...)
	b = append(b, le32(baseDate)...)
	return append(b, action...)
}

// outgoing sets the out flag of a service blob (flags word at offset 4).
func outgoing(b []byte) []byte {
	b[4] |= bitOut
	return b
}

type callWant struct {
	table, row, dir, outcome, kind string
	label, slug, address           string
	duration                       int64 // -1: none
	reasonState                    bool
}

// Q6, spec 8.7, T6 (Q5): each phone-call row (R11-R17 of v181, P6 of v64) is one call record and no message; the
// outcome maps only Missed and Busy of an incoming call, every other reason or direction is unknown with its label,
// never answered; R12's duration is seconds (facts.tsv unit.call_duration), its end is start plus it.
func TestTelegramCallFromPhoneCallAction(t *testing.T) {
	wants := map[string][]callWant{
		"telegram-v181": {
			{table: "messages_v2", row: "11", dir: "in", outcome: "missed", kind: "voice", address: "user:1000002", duration: -1},
			{table: "messages_v2", row: "12", dir: "out", outcome: "unknown", kind: "video", label: "hangup", address: "user:1000002", duration: 125},
			{table: "messages_v2", row: "13", dir: "in", outcome: "rejected", kind: "voice", address: "user:1000002", duration: -1},
			{table: "messages_v2", row: "14", dir: "in", outcome: "unknown", kind: "voice", address: "user:1000002", duration: 60, reasonState: true},
			{table: "messages_v2", row: "15", dir: "in", outcome: "unknown", kind: "voice", label: "disconnect", address: "user:1000002", duration: -1},
			{table: "messages_v2", row: "16", dir: "in", outcome: "unknown", kind: "voice", label: "migrate_conference_call", slug: "conf-1", address: "user:1000002", duration: -1},
			{table: "messages_v2", row: "17", dir: "in", outcome: "missed", kind: "voice", duration: -1},
		},
		"telegram-v64": {
			{table: "messages", row: "107", dir: "in", outcome: "missed", kind: "voice", address: "user:1000002", duration: -1},
		},
	}
	for variant, ws := range wants {
		res := parseFixture(t, variant)
		perRow := map[string]int{}
		for _, r := range res.Records {
			perRow[locatorRow(r.Locator)]++
			if r.Type == "call" && r.Payload["outcome"] == "answered" {
				t.Errorf("%s: %s is answered: never inferred (Q5)", variant, r.Locator)
			}
		}
		rows := byRow(res.Records)
		for _, w := range ws {
			key := w.table + "/" + w.row
			r, ok := rows[key]
			if !ok || perRow[key] != 1 || r.Type != "call" {
				t.Errorf("%s %s: %d records, type %q, want one call and no message", variant, key, perRow[key], r.Type)
				continue
			}
			p, raw := r.Payload, rawOf(r)
			if p["direction"] != w.dir || p["outcome"] != w.outcome || p["kind"] != w.kind || p["service"] != "telegram" {
				t.Errorf("%s %s: direction %v outcome %v kind %v service %v", variant, key, p["direction"], p["outcome"], p["kind"], p["service"])
			}
			if raw["label_reason"] != anyOrNil(w.label) || raw["slug"] != anyOrNil(w.slug) {
				t.Errorf("%s %s: label_reason %v slug %v, want %q %q", variant, key, raw["label_reason"], raw["slug"], w.label, w.slug)
			}
			if (raw["reason_state"] == "not-recorded") != w.reasonState {
				t.Errorf("%s %s: reason_state %v", variant, key, raw["reason_state"])
			}
			if got, _ := p["address"].(string); got != w.address {
				t.Errorf("%s %s: address %q, want %q", variant, key, got, w.address)
			}
			if w.address == "" && raw["address_state"] != "not-user-dialog" {
				t.Errorf("%s %s: address_state %v", variant, key, raw["address_state"])
			}
			if _, has := raw["reason"]; has == w.reasonState {
				t.Errorf("%s %s: raw.reason present %v", variant, key, has)
			}
			checkCallDuration(t, variant+" "+key, r, w.duration)
		}
	}
}

func anyOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// checkCallDuration states the duration rule of spec 8.7 for the pinned unit: duration_s and TimeEnd = Time + duration
// when unitCallDuration is "seconds", else raw.duration with duration_unit "unestablished" and no TimeEnd.
func checkCallDuration(t *testing.T, name string, r records.Record, d int64) {
	t.Helper()
	raw, p := rawOf(r), r.Payload
	if d < 0 {
		if _, has := p["duration_s"]; has || r.TimeEnd != nil || raw["duration"] != nil {
			t.Errorf("%s: a call without a duration has duration_s %v, TimeEnd %v, raw.duration %v", name, p["duration_s"], r.TimeEnd, raw["duration"])
		}
		return
	}
	if jsonText(raw["duration"]) != itoa(d) {
		t.Errorf("%s: raw.duration %v, want %d", name, raw["duration"], d)
	}
	if androidtelegram.UnitCallDuration == "seconds" {
		if jsonText(p["duration_s"]) != itoa(d) {
			t.Errorf("%s: duration_s %v, want %d", name, p["duration_s"], d)
		}
		if r.Time == nil || r.TimeEnd == nil || !r.TimeEnd.T.Equal(r.Time.T.Add(time.Duration(d)*time.Second)) || raw["time_end_basis"] != "start+duration" {
			t.Errorf("%s: TimeEnd %v, want Time %v + %ds (basis %v)", name, r.TimeEnd, r.Time, d, raw["time_end_basis"])
		}
		return
	}
	if _, has := p["duration_s"]; has || r.TimeEnd != nil || raw["duration_unit"] != "unestablished" {
		t.Errorf("%s: unpinned unit: duration_s %v, TimeEnd %v, duration_unit %v", name, p["duration_s"], r.TimeEnd, raw["duration_unit"])
	}
}

// Spec 8.7 (recon 3.5): a call whose action carries no discard reason is unknown with raw.reason_state
// "not-recorded", even with a duration (R14: 60 s): nothing is inferred from the duration. Go-built rows add the
// direction rule: Missed and Busy of an outgoing row are unknown with their label.
func TestTelegramAbsentDiscardReasonIsNotRecorded(t *testing.T) {
	res := parseFixture(t, "telegram-v181")
	r14 := byRow(res.Records)["messages_v2/14"]
	raw := rawOf(r14)
	if r14.Type != "call" || r14.Payload["outcome"] != "unknown" || raw["reason_state"] != "not-recorded" || jsonText(raw["duration"]) != "60" {
		t.Errorf("R14: type %s outcome %v reason_state %v duration %v", r14.Type, r14.Payload["outcome"], raw["reason_state"], raw["duration"])
	}

	const ann = 1000002
	sixty, zero := int32(60), int32(0)
	cells := []msgCells{
		{mid: 1, uid: ann, data: tlService(1, ann, peerUser(1000001), tlCall{id: 1, duration: &sixty}.bytes())},
		{mid: 2, uid: ann, out: int64(1), data: outgoing(tlService(2, 0, peerUser(ann), tlCall{id: 2, reason: le32(idReasonMissed)}.bytes()))},
		{mid: 3, uid: ann, out: int64(1), data: outgoing(tlService(3, 0, peerUser(ann), tlCall{id: 3, reason: le32(idReasonBusy)}.bytes()))},
		{mid: 4, uid: ann, data: tlService(4, ann, peerUser(1000001), tlCall{id: 4, reason: le32(idReasonHangup), duration: &zero}.bytes())},
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)
	want := []struct {
		row, dir, label, state string
		end                    bool
	}{
		{"1", "in", "", "not-recorded", true},
		{"2", "out", "missed", "", false},
		{"3", "out", "busy", "", false},
		{"4", "in", "hangup", "", false},
	}
	for _, w := range want {
		r := got["messages_v2/"+w.row]
		raw := rawOf(r)
		if r.Type != "call" || r.Payload["outcome"] != "unknown" || r.Payload["direction"] != w.dir {
			t.Errorf("row %s: type %s outcome %v direction %v, want %s", w.row, r.Type, r.Payload["outcome"], r.Payload["direction"], w.dir)
		}
		if raw["label_reason"] != anyOrNil(w.label) || raw["reason_state"] != anyOrNil(w.state) {
			t.Errorf("row %s: label_reason %v reason_state %v", w.row, raw["label_reason"], raw["reason_state"])
		}
		if (r.TimeEnd != nil) != w.end {
			t.Errorf("row %s: TimeEnd %v, want present %v (a zero duration has no end)", w.row, r.TimeEnd, w.end)
		}
	}
}

// Spec 8.7 (Q7): a phone-call action whose blob stops before the action is one message of kind unknown with its
// state, never a guessed call; R18 (a conference call, an ID the table lacks) is a system message, partial.
func TestTelegramCallBlobStoppedBeforeActionIsAMessage(t *testing.T) {
	cells := []msgCells{{mid: 1, uid: 1000002, data: tlService(1, 1000002, peerUser(1000001), nil)}}
	got := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records
	if len(got) != 1 || got[0].Type != "message" || got[0].Payload["kind"] != "unknown" || rawOf(got[0])["data_state"] != "truncated" {
		t.Fatalf("a service blob cut before its action: %d records %+v", len(got), got)
	}
	r18 := byRow(parseFixture(t, "telegram-v181").Records)["messages_v2/18"]
	if r18.Type != "message" || r18.Payload["kind"] != "system" || rawOf(r18)["data_state"] != "partial" {
		t.Errorf("R18: type %s kind %v data_state %v", r18.Type, r18.Payload["kind"], rawOf(r18)["data_state"])
	}
}
