package message_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func ptr[T any](v T) *T { return &v }

// minimal is the smallest message the contract accepts.
func minimal() message.Message {
	return message.Message{
		Channel: message.ChannelSMS, Direction: message.DirUnknown, Kind: message.KindUnknown,
		ParticipantsUnknown: true,
	}
}

func full() message.Message {
	return message.Message{
		Channel: message.ChannelIMessage, Direction: message.DirIn, Kind: message.KindText,
		Participants: []message.Participant{
			{Role: message.RoleFrom, Address: "+15551234567", Name: "Alex", Kind: "phone"},
			{Role: message.RoleTo, Address: "me@example.com", Kind: "email"},
		},
		Thread:  &message.Thread{ID: "chat123", Title: "Plans", Group: ptr(false), Members: []string{"+15551234567", "me@example.com"}},
		Status:  message.StatusReceived,
		Read:    ptr(false),
		Subject: "hi",
		Attachments: []message.Attachment{{
			Name: "a.jpg", Mime: "image/jpeg", Size: ptr(int64(0)), SourceRef: "Library/SMS/a.jpg", ArtifactID: "art-1", ArtifactSHA256: strings.Repeat("ab", 32),
		}},
		GUID:           "G-1",
		TargetGUID:     "G-0",
		Service:        "iMessage",
		Subscription:   ptr(int64(-1)),
		TextSource:     message.TextSourceAttributedBody,
		BodyFlags:      []string{"invalid_utf8", "truncated"},
		BodyTruncated:  ptr(true),
		BodyTotalBytes: ptr(int64(123456)),
		BodyRawSHA256:  strings.Repeat("0f", 32),
		BodyRawLen:     ptr(int64(123456)),
		Deleted:        map[string]any{"source": "sqlite-freelist"},
		Raw:            map[string]any{"flags": int64(5), "nested": map[string]any{"a": []any{"x", int64(2)}}},
	}
}

func groupMMS() message.Message {
	return message.Message{
		Channel: message.ChannelMMS, Direction: message.DirOut, Kind: message.KindMedia,
		Participants: []message.Participant{
			{Role: message.RoleFrom, Address: "+15550000000"},
			{Role: message.RoleMember, Address: "+15551111111"},
			{Role: message.RoleMember, Address: "+15552222222"},
			{Role: message.RoleBCC, Address: "ops@example.com"},
		},
		Thread:       &message.Thread{ID: "42", Group: ptr(true), Members: []string{"+15551111111", "+15552222222"}},
		Status:       message.StatusSent,
		Subscription: ptr(int64(2)),
		Attachments:  []message.Attachment{{Mime: "image/png"}, {Name: "b.png", Size: ptr(int64(2048))}},
	}
}

// TestPayloadValidatorsAcceptParserOutput (message part): what the builder makes
// validates, and a real records.Writer over a temp case accepts it.
func TestPayloadValidatorsAcceptParserOutput(t *testing.T) {
	msgs := map[string]message.Message{"minimal": minimal(), "full": full(), "group MMS": groupMMS()}
	for name, m := range msgs {
		t.Run(name, func(t *testing.T) {
			if err := message.Validate(m.Payload()); err != nil {
				t.Fatalf("Validate(builder output) = %v", err)
			}
		})
	}

	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "sms.db", make([]byte, 1<<16))
	w, err := records.NewWriter(c, records.Parser{Name: "message-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for name, m := range msgs {
		rec := records.Record{Type: message.Type, ArtifactID: a.ID, Summary: message.Summary(m.Channel, m.Direction, "someone", "text"), Payload: m.Payload()}
		if err := w.Add(ctx, rec); err != nil {
			t.Errorf("the writer refused the %s message: %v", name, err)
		}
	}
	// the same writer refuses a violation with the typed error
	bad := records.Record{Type: message.Type, ArtifactID: a.ID, Summary: "bad", Payload: map[string]any{"channel": "sms"}}
	if err := w.Add(ctx, bad); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(invalid message) = %v, want ErrInvalidPayload", err)
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(msgs)) || res.Rejected != 1 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

func TestMessageRegisteredValidator(t *testing.T) {
	for _, ti := range records.Types() {
		if ti.Name == message.Type {
			if !ti.HasValidator {
				t.Error("message has no validator although its package is imported")
			}
			if ti.PayloadVersion != message.PayloadVersion {
				t.Errorf("registered payload version %d, package says %d", ti.PayloadVersion, message.PayloadVersion)
			}
			return
		}
	}
	t.Error("message is not a registered type")
}

// mutate returns the full message's payload after fn changed it.
func mutate(fn func(p map[string]any)) map[string]any {
	p := full().Payload()
	fn(p)
	return p
}

func partOf(p map[string]any, i int) map[string]any {
	return p["participants"].([]any)[i].(map[string]any)
}

func attOf(p map[string]any, i int) map[string]any {
	return p["attachments"].([]any)[i].(map[string]any)
}
func threadOf(p map[string]any) map[string]any { return p["thread"].(map[string]any) }

func makeAtts(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"mime": "text/plain"}
	}
	return out
}

// TestPayloadValidatorsRejectViolations (message part): one row per F10 rule.
// Every error names the path (and the reason), and none repeats a value.
func TestPayloadValidatorsRejectViolations(t *testing.T) {
	const marker = "SECRET-VALUE-4417"
	type row struct {
		name string
		p    map[string]any
		want string
	}
	var rows []row
	add := func(name string, p map[string]any, want string) { rows = append(rows, row{name, p, want}) }

	// required fields, one by one
	for _, f := range []string{"channel", "direction", "kind", "participants"} {
		add("missing "+f, mutate(func(p map[string]any) { delete(p, f) }), "payload."+f+": required field is missing")
	}
	add("nil payload", nil, "payload.channel: required field is missing")
	add("participant without role", mutate(func(p map[string]any) { delete(partOf(p, 0), "role") }), "payload.participants[0].role: required field is missing")
	add("participant without address", mutate(func(p map[string]any) { delete(partOf(p, 1), "address") }), "payload.participants[1].address: required field is missing")

	// enums and tokens
	for _, bad := range []string{"SMS", "", "1sms", "sms-2", "sms sms", strings.Repeat("a", 33), "smé"} {
		add(fmt.Sprintf("channel %q", bad), mutate(func(p map[string]any) { p["channel"] = bad }), "payload.channel")
	}
	add("channel not a string", mutate(func(p map[string]any) { p["channel"] = int64(1) }), "payload.channel")
	add("direction enum", mutate(func(p map[string]any) { p["direction"] = "inbound" }), "payload.direction: not one of in,out,unknown")
	add("direction case", mutate(func(p map[string]any) { p["direction"] = "IN" }), "payload.direction")
	add("kind enum", mutate(func(p map[string]any) { p["kind"] = "image" }), "payload.kind: not one of text,media,reaction,system,unknown")
	add("role enum", mutate(func(p map[string]any) { partOf(p, 0)["role"] = "sender" }), "payload.participants[0].role: not one of from,to,cc,bcc,member")
	add("participant kind enum", mutate(func(p map[string]any) { partOf(p, 0)["kind"] = "fax" }), "payload.participants[0].kind: not one of phone,email,shortcode,alnum,unknown")
	add("status enum", mutate(func(p map[string]any) { p["status"] = "read" }), "payload.status: not one of received,sent,delivered,failed,queued,draft,unknown")
	add("text_source enum", mutate(func(p map[string]any) { p["text_source"] = "html" }), "payload.text_source: not one of text,attributedBody,part,summary")

	// participants
	add("empty participants without participants_unknown", mutate(func(p map[string]any) {
		p["participants"] = []any{}
		delete(p, "participants_unknown")
	}), "participants is empty without participants_unknown")
	add("empty participants with participants_unknown false", mutate(func(p map[string]any) {
		p["participants"] = []any{}
		p["participants_unknown"] = false
	}), "participants is empty without participants_unknown")
	add("empty address", mutate(func(p map[string]any) { partOf(p, 0)["address"] = "" }), "payload.participants[0].address")
	add("address over 1024 bytes", mutate(func(p map[string]any) { partOf(p, 0)["address"] = strings.Repeat("9", 1025) }), "payload.participants[0].address")
	add("participant not an object", mutate(func(p map[string]any) { p["participants"] = []any{"x"} }), "payload.participants[0]")
	add("participants not an array", mutate(func(p map[string]any) { p["participants"] = map[string]any{} }), "payload.participants")

	// attachments
	add("attachment sha without id", mutate(func(p map[string]any) { delete(attOf(p, 0), "artifact_id") }), "artifact_sha256 requires artifact_id")
	add("attachment sha uppercase", mutate(func(p map[string]any) { attOf(p, 0)["artifact_sha256"] = strings.Repeat("AB", 32) }), "payload.attachments[0].artifact_sha256")
	add("attachment sha short", mutate(func(p map[string]any) { attOf(p, 0)["artifact_sha256"] = "abc" }), "payload.attachments[0].artifact_sha256")
	add("attachment sha not hex", mutate(func(p map[string]any) { attOf(p, 0)["artifact_sha256"] = strings.Repeat("zz", 32) }), "payload.attachments[0].artifact_sha256")
	add("attachment size negative", mutate(func(p map[string]any) { attOf(p, 0)["size"] = int64(-1) }), "payload.attachments[0].size")
	add("attachment size not an integer", mutate(func(p map[string]any) { attOf(p, 0)["size"] = "12" }), "payload.attachments[0].size")
	for _, f := range []string{"name", "mime", "source_ref", "artifact_id"} {
		add("attachment "+f+" type", mutate(func(p map[string]any) { attOf(p, 0)[f] = int64(1) }), "payload.attachments[0]."+f)
	}
	add("attachment not an object", mutate(func(p map[string]any) { p["attachments"] = []any{int64(1)} }), "payload.attachments[0]")

	// body fields
	add("body_truncated without total", mutate(func(p map[string]any) { delete(p, "body_total_bytes") }), "body_truncated requires body_total_bytes")
	add("body_flags unknown entry", mutate(func(p map[string]any) { p["body_flags"] = []any{"invalid_utf8", marker} }), "body_flags")
	add("body_total_bytes negative", mutate(func(p map[string]any) { p["body_total_bytes"] = int64(-5) }), "payload.body_total_bytes")
	add("body_raw_len negative", mutate(func(p map[string]any) { p["body_raw_len"] = int64(-5) }), "payload.body_raw_len")
	add("body_raw_sha256 not hex", mutate(func(p map[string]any) { p["body_raw_sha256"] = "xyz" }), "payload.body_raw_sha256")

	// wrong types, each optional field
	wrong := map[string]any{
		"participants_unknown": "yes", "thread": "x", "status": int64(1), "read": "true", "subject": int64(1), "attachments": map[string]any{},
		"guid": int64(1), "target_guid": []any{}, "service": true, "subscription": "1", "text_source": int64(1), "body_flags": "x",
		"body_truncated": int64(1), "body_total_bytes": "1", "body_raw_b64": int64(1), "body_raw_sha256": int64(1), "body_raw_len": "x",
		"deleted": "x", "raw": "x", "recovery": "x", "snapshot": "x",
	}
	names := make([]string, 0, len(wrong))
	for k := range wrong {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, f := range names {
		add("wrong type for "+f, mutate(func(p map[string]any) { p[f] = wrong[f] }), "payload."+f)
	}
	add("thread.id wrong type", mutate(func(p map[string]any) { threadOf(p)["id"] = []any{} }), "payload.thread.id")
	add("thread.title wrong type", mutate(func(p map[string]any) { threadOf(p)["title"] = int64(1) }), "payload.thread.title")
	add("thread.group wrong type", mutate(func(p map[string]any) { threadOf(p)["group"] = "x" }), "payload.thread.group")
	add("thread.members wrong type", mutate(func(p map[string]any) { threadOf(p)["members"] = "x" }), "payload.thread.members")
	add("thread.members entry wrong type", mutate(func(p map[string]any) { threadOf(p)["members"] = []any{int64(1)} }), "payload.thread.members")
	add("participant name wrong type", mutate(func(p map[string]any) { partOf(p, 0)["name"] = int64(1) }), "payload.participants[0].name")
	add("deleted without source", mutate(func(p map[string]any) { p["deleted"] = map[string]any{} }), "payload.deleted.source")
	add("recovery relation", mutate(func(p map[string]any) { p["recovery"] = map[string]any{"relation": "found"} }), "payload.recovery.relation")
	add("snapshot without name", mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"xid": int64(1)} }), "payload.snapshot.name")

	// depth and size
	add("raw nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{}}}}
	}), "payload.raw")
	add("raw array nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": []any{[]any{[]any{int64(1)}}}}
	}), "payload.raw")
	add("attachments array of 10,001", mutate(func(p map[string]any) { p["attachments"] = make([]any, 10001) }), "payload.attachments")
	add("participants array of 10,001", mutate(func(p map[string]any) {
		parts := make([]any, 10001)
		for i := range parts {
			parts[i] = map[string]any{"role": "to", "address": "x"}
		}
		p["participants"] = parts
	}), "payload.participants")
	add("members array of 10,001", mutate(func(p map[string]any) { threadOf(p)["members"] = make([]any, 10001) }), "payload.thread.members")

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := message.Validate(r.p)
			if err == nil {
				t.Fatalf("an invalid payload was accepted")
			}
			if !strings.Contains(err.Error(), r.want) {
				t.Errorf("error %q does not contain %q", err, r.want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error repeats a payload value: %q", err)
			}
		})
	}

	// the boundaries that are accepted
	ok := map[string]map[string]any{
		"raw at depth 4": mutate(func(p map[string]any) {
			p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": int64(1)}}}
		}),
		"10,000 attachments":          mutate(func(p map[string]any) { p["attachments"] = makeAtts(10000) }),
		"address of 1024 bytes":       mutate(func(p map[string]any) { partOf(p, 0)["address"] = strings.Repeat("9", 1024) }),
		"empty participants, unknown": mutate(func(p map[string]any) { p["participants"] = []any{}; p["participants_unknown"] = true }),
		"unknown fields": mutate(func(p map[string]any) {
			p["future_field"] = map[string]any{"x": int64(1)}
			partOf(p, 0)["future"] = true
		}),
		"unknown enum word":    mutate(func(p map[string]any) { p["direction"] = "unknown"; p["kind"] = "unknown"; p["status"] = "unknown" }),
		"app channel":          mutate(func(p map[string]any) { p["channel"] = "whatsapp" }),
		"body_truncated false": mutate(func(p map[string]any) { delete(p, "body_total_bytes"); p["body_truncated"] = false }),
		"thread id as integer": mutate(func(p map[string]any) { threadOf(p)["id"] = int64(7) }),
		"json.Number integers": mutate(func(p map[string]any) { p["subscription"] = json.Number("3"); attOf(p, 0)["size"] = json.Number("10") }),
	}
	for name, p := range ok {
		if err := message.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestMessageBuilderOmitsAbsent (C3): an empty string and a nil pointer never
// appear; a set pointer does, even when it holds false or 0.
func TestMessageBuilderOmitsAbsent(t *testing.T) {
	p := minimal().Payload()
	want := map[string]any{"channel": "sms", "direction": "unknown", "kind": "unknown", "participants": []any{}, "participants_unknown": true}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("minimal payload = %#v, want %#v", p, want)
	}

	// unknown participants false is absent, not false
	m := minimal()
	m.ParticipantsUnknown = false
	m.Participants = []message.Participant{{Role: "from", Address: "x"}}
	if _, has := m.Payload()["participants_unknown"]; has {
		t.Error("participants_unknown:false was written")
	}

	// set pointers appear even when false / zero
	m = minimal()
	m.Read, m.Subscription, m.BodyTruncated, m.BodyTotalBytes, m.BodyRawLen = ptr(false), ptr(int64(0)), ptr(false), ptr(int64(0)), ptr(int64(0))
	m.Thread = &message.Thread{Group: ptr(false)}
	m.Attachments = []message.Attachment{{Size: ptr(int64(0))}}
	p = m.Payload()
	for _, k := range []string{"read", "subscription", "body_truncated", "body_total_bytes", "body_raw_len"} {
		if _, has := p[k]; !has {
			t.Errorf("%s was omitted although set", k)
		}
	}
	if p["read"] != false || p["subscription"] != int64(0) {
		t.Errorf("read %v subscription %v", p["read"], p["subscription"])
	}
	if th := threadOf(p); len(th) != 1 || th["group"] != false {
		t.Errorf("thread = %#v, want only group:false", th)
	}
	if att := attOf(p, 0); len(att) != 1 || att["size"] != int64(0) {
		t.Errorf("attachment = %#v, want only size:0", att)
	}

	// nothing empty anywhere in a full payload
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if x == "" {
				t.Errorf("%s is an empty string", path)
			}
		case map[string]any:
			if len(x) == 0 {
				t.Errorf("%s is an empty object", path)
			}
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("payload", full().Payload())
	walk("payload", groupMMS().Payload())

	// a thread without a title or members carries neither
	if _, has := threadOf(groupMMS().Payload())["title"]; has {
		t.Error("an empty title was written")
	}
	// a message that holds nothing is an invalid payload: the omission is the builder's, the check the validator's
	if err := message.Validate(message.Message{}.Payload()); err == nil {
		t.Error("an empty message validated")
	}
}

// TestMessagePayloadIsCanonicalTypes: only the types the records package
// canonicalizes, and the payload does not alias the Message.
func TestMessagePayloadIsCanonicalTypes(t *testing.T) {
	for name, m := range map[string]message.Message{"minimal": minimal(), "full": full(), "group": groupMMS()} {
		if !canonical(m.Payload()) {
			t.Errorf("%s payload holds a type outside string, bool, int64, []any, map[string]any", name)
		}
	}
	m := full()
	p := m.Payload()
	p["deleted"].(map[string]any)["source"] = "changed"
	p["raw"].(map[string]any)["flags"] = "changed"
	p["raw"].(map[string]any)["nested"].(map[string]any)["a"].([]any)[0] = "changed"
	if m.Deleted["source"] != "sqlite-freelist" || m.Raw["flags"] != int64(5) || m.Raw["nested"].(map[string]any)["a"].([]any)[0] != "x" {
		t.Error("the payload aliases the message's deleted/raw maps")
	}
	// two calls give independent maps
	a, b := full().Payload(), full().Payload()
	partOf(a, 0)["address"] = "changed"
	if partOf(b, 0)["address"] == "changed" {
		t.Error("payloads share state")
	}
}

func canonical(v any) bool {
	switch x := v.(type) {
	case string, bool, int64:
		return true
	case []any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	}
	return false
}

func TestSummary(t *testing.T) {
	cases := []struct{ channel, dir, who, text, want string }{
		{"sms", "in", "+15551234567", "hello there", "sms in +15551234567: hello there"},
		{"imessage", "out", "Alex", "a\nb\tc", "imessage out Alex: a b c"},
		{"sms", "in", "x", "", "sms in x:"},
	}
	for _, tc := range cases {
		if got := message.Summary(tc.channel, tc.dir, tc.who, tc.text); got != tc.want {
			t.Errorf("Summary(%q,%q,%q,%q) = %q, want %q", tc.channel, tc.dir, tc.who, tc.text, got, tc.want)
		}
	}
	long := message.Summary("sms", "in", "x", strings.Repeat("word ", 100))
	if want := "sms in x: "; !strings.HasPrefix(long, want) || len(long)-len(want) > 80 || !strings.HasSuffix(long, "...") {
		t.Errorf("long text summary = %q", long)
	}
	// hostile parts stay one line and bounded
	rlo := string(rune(0x202E))
	h := message.Summary(strings.Repeat("c", 1000), "in\n", strings.Repeat("é", 1000)+"\x00", rlo+"evil")
	if strings.ContainsAny(h, "\n\x00"+rlo) || len(h) > 32+16+64+4+80 {
		t.Errorf("hostile summary = %q (%d bytes)", h, len(h))
	}
}

// FuzzMessageValidate: arbitrary JSON into Validate never panics, and an accepted
// payload always decodes (and decodes to something that validates again).
func FuzzMessageValidate(f *testing.F) {
	for _, m := range []message.Message{minimal(), full(), groupMMS()} {
		b, err := json.Marshal(m.Payload())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"channel":"sms","direction":"in","kind":"text","participants":[{"role":"from","address":"x"}],"raw":{"a":{"b":{"c":{}}}}}`))
	f.Add([]byte(`{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"subscription":1e400}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return
		}
		if err := message.Validate(m); err != nil {
			if len(err.Error()) > 1<<12 {
				t.Errorf("error too long: %d bytes", len(err.Error()))
			}
			return
		}
		got, err := message.Decode(message.PayloadVersion, data)
		if err != nil {
			// only trailing data after the first value can make a validated object fail to decode
			if len(strings.TrimSpace(string(data[dec.InputOffset():]))) == 0 {
				t.Fatalf("an accepted payload does not decode: %v", err)
			}
			return
		}
		again, err := message.Decode(message.PayloadVersion, data)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatalf("Decode is not deterministic: %v", err)
		}
		if err := message.Validate(got.Payload()); err != nil {
			t.Fatalf("the decoded message does not produce a valid payload: %v", err)
		}
	})
}
