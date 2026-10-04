package call_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

func ptr[T any](v T) *T { return &v }

// minimal is the smallest call the contract accepts.
func minimal() call.Call { return call.Call{Direction: "unknown", Outcome: "unknown"} }

func full() call.Call {
	return call.Call{
		Direction: "in", Outcome: "missed", Address: "+15551234567", AddressPresentation: "allowed", NameCached: "Alex",
		Kind: "video", Service: "cellular", CountryISO: "US", GeoDescription: "California",
		DurationS: ptr(0.5), Subscription: ptr(int64(-1)), Read: ptr(false), New: ptr(true),
		Raw:      map[string]any{"type": int64(3), "features": int64(0), "nested": map[string]any{"a": []any{"x", int64(2), 1.5}}},
		Recovery: map[string]any{"relation": "absent-from-live", "via": "wal", "wal": map[string]any{"frame": int64(3), "committed": true}},
		Snapshot: map[string]any{"name": "com.apple.snap", "xid": int64(12)},
		Deleted:  map[string]any{"source": "calls.deleted"},
	}
}

// hidden is a call from a withheld number: no address, zero seconds set.
func hidden() call.Call {
	return call.Call{
		Direction: "in", Outcome: "rejected", AddressPresentation: "restricted",
		DurationS: ptr(0.0), Subscription: ptr(int64(0)), Read: ptr(false), New: ptr(false),
	}
}

// TestPayloadValidatorsAcceptParserOutput (call part): what the builder makes
// validates, and a real records.Writer over a temp case accepts it.
func TestPayloadValidatorsAcceptParserOutput(t *testing.T) {
	calls := map[string]call.Call{"minimal": minimal(), "full": full(), "hidden": hidden()}
	for name, c := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call.Validate(c.Payload()); err != nil {
				t.Fatalf("Validate(builder output) = %v", err)
			}
		})
	}

	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "calllog.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "call-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for name, c := range calls {
		rec := records.Record{Type: call.Type, ArtifactID: a.ID, Summary: call.Summary(c.Direction, c.Outcome, c.Address), Payload: c.Payload()}
		if err := w.Add(ctx, rec); err != nil {
			t.Errorf("the writer refused the %s call: %v", name, err)
		}
	}
	// the same writer refuses a violation with the typed error
	bad := records.Record{Type: call.Type, ArtifactID: a.ID, Summary: "bad", Payload: map[string]any{"direction": "in"}}
	if err := w.Add(ctx, bad); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(invalid call) = %v, want ErrInvalidPayload", err)
	}
	// a negative duration is refused through the writer too
	neg := full().Payload()
	neg["duration_s"] = float64(-1)
	if err := w.Add(ctx, records.Record{Type: call.Type, ArtifactID: a.ID, Summary: "neg", Payload: neg}); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(negative duration) = %v, want ErrInvalidPayload", err)
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(calls)) || res.Rejected != 2 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

func TestCallRegisteredValidator(t *testing.T) {
	for _, ti := range records.Types() {
		if ti.Name == call.Type {
			if !ti.HasValidator {
				t.Error("call has no validator although its package is imported")
			}
			if ti.PayloadVersion != call.PayloadVersion {
				t.Errorf("registered payload version %d, package says %d", ti.PayloadVersion, call.PayloadVersion)
			}
			return
		}
	}
	t.Error("call is not a registered type")
}

func mutate(fn func(p map[string]any)) map[string]any {
	p := full().Payload()
	fn(p)
	return p
}

// TestPayloadValidatorsRejectViolations (call part): one row per F10 rule.
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

	for _, f := range []string{"direction", "outcome"} {
		add("missing "+f, mutate(func(p map[string]any) { delete(p, f) }), "payload."+f+": required field is missing")
	}
	add("nil payload", nil, "payload.direction: required field is missing")
	add("empty payload", map[string]any{}, "payload.direction: required field is missing")
	add("only direction", map[string]any{"direction": "in"}, "payload.outcome: required field is missing")

	// enums
	add("direction enum", mutate(func(p map[string]any) { p["direction"] = "incoming" }), "payload.direction: not one of in,out,unknown")
	add("direction case", mutate(func(p map[string]any) { p["direction"] = "IN" }), "payload.direction")
	add("direction type", mutate(func(p map[string]any) { p["direction"] = int64(1) }), "payload.direction")
	add("outcome enum", mutate(func(p map[string]any) { p["outcome"] = "ringing" }), "payload.outcome: not one of answered,missed,rejected,blocked,voicemail,cancelled,unknown")
	add("outcome empty", mutate(func(p map[string]any) { p["outcome"] = "" }), "payload.outcome")
	add("address_presentation enum", mutate(func(p map[string]any) { p["address_presentation"] = "hidden" }), "payload.address_presentation: not one of allowed,restricted,unknown,payphone")
	add("kind enum", mutate(func(p map[string]any) { p["kind"] = "audio" }), "payload.kind: not one of voice,video,facetime_audio,facetime_video,unknown")

	// duration
	add("duration negative", mutate(func(p map[string]any) { p["duration_s"] = float64(-1) }), "payload.duration_s: below the minimum 0")
	add("duration negative int", mutate(func(p map[string]any) { p["duration_s"] = int64(-1) }), "payload.duration_s")
	add("duration negative fraction", mutate(func(p map[string]any) { p["duration_s"] = -0.5 }), "payload.duration_s")
	add("duration NaN", mutate(func(p map[string]any) { p["duration_s"] = math.NaN() }), "payload.duration_s")
	add("duration +Inf", mutate(func(p map[string]any) { p["duration_s"] = math.Inf(1) }), "payload.duration_s")
	add("duration -Inf", mutate(func(p map[string]any) { p["duration_s"] = math.Inf(-1) }), "payload.duration_s")
	add("duration string", mutate(func(p map[string]any) { p["duration_s"] = "30" }), "payload.duration_s")
	add("duration json.Number negative", mutate(func(p map[string]any) { p["duration_s"] = json.Number("-1") }), "payload.duration_s")
	add("duration json.Number NaN", mutate(func(p map[string]any) { p["duration_s"] = json.Number("NaN") }), "payload.duration_s")
	add("duration json.Number overflow", mutate(func(p map[string]any) { p["duration_s"] = json.Number("1e999") }), "payload.duration_s")

	// wrong types, each optional field
	wrong := map[string]any{
		"address": int64(1), "address_presentation": int64(1), "name_cached": []any{}, "duration_s": true, "kind": int64(1),
		"service": true, "country_iso": int64(1), "geo_description": map[string]any{}, "subscription": "1", "read": "true", "new": int64(1),
		"raw": "x", "recovery": "x", "snapshot": "x", "deleted": "x",
	}
	names := make([]string, 0, len(wrong))
	for k := range wrong {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, f := range names {
		add("wrong type for "+f, mutate(func(p map[string]any) { p[f] = wrong[f] }), "payload."+f)
	}
	add("subscription fractional", mutate(func(p map[string]any) { p["subscription"] = json.Number("1.5") }), "payload.subscription")
	// integers are canonical JSON integers: a decimal or exponent form is refused (Decode could not read it back)
	for _, in := range []string{"1e3", "2.0", "1E3", "10e-1", "0.0"} {
		add("subscription as "+in, mutate(func(p map[string]any) { p["subscription"] = json.Number(in) }), "payload.subscription")
		add("snapshot xid as "+in, mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "s", "xid": json.Number(in)} }), "payload.snapshot.xid")
	}
	add("deleted without source", mutate(func(p map[string]any) { p["deleted"] = map[string]any{} }), "payload.deleted.source")
	add("recovery relation", mutate(func(p map[string]any) { p["recovery"] = map[string]any{"relation": "found"} }), "payload.recovery.relation")
	add("snapshot without name", mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"xid": int64(1)} }), "payload.snapshot.name")
	add("snapshot negative xid", mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "s", "xid": int64(-1)} }), "payload.snapshot.xid")

	// depth, scalars of raw
	add("raw nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{}}}}
	}), "payload.raw")
	add("raw array nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": []any{[]any{[]any{int64(1)}}}}
	}), "payload.raw")
	add("raw holds NaN", mutate(func(p map[string]any) { p["raw"] = map[string]any{"x": math.NaN()} }), "payload.raw")
	add("raw holds an unsupported type", mutate(func(p map[string]any) { p["raw"] = map[string]any{"x": struct{}{}} }), "payload.raw")
	add("raw array of 10,001", mutate(func(p map[string]any) { p["raw"] = map[string]any{"x": make([]any, 10001)} }), "payload.raw")
	add("marker is never echoed", mutate(func(p map[string]any) { p["outcome"] = marker }), "payload.outcome")

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := call.Validate(r.p)
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
		"raw array of 10,000": mutate(func(p map[string]any) { p["raw"] = map[string]any{"x": make([]any, 10000)} }),
		"unknown fields": mutate(func(p map[string]any) {
			p["future_field"] = map[string]any{"x": int64(1)}
		}),
		"unknown enum word": mutate(func(p map[string]any) {
			p["direction"], p["outcome"], p["address_presentation"], p["kind"] = "unknown", "unknown", "unknown", "unknown"
		}),
		"every outcome": mutate(func(p map[string]any) { p["outcome"] = "voicemail" }),
		"payphone":      mutate(func(p map[string]any) { p["address_presentation"] = "payphone" }),
		"facetime":      mutate(func(p map[string]any) { p["kind"] = "facetime_audio" }),
		"no address":    mutate(func(p map[string]any) { delete(p, "address") }),
		"json.Number integers": mutate(func(p map[string]any) {
			p["subscription"] = json.Number("3")
			p["duration_s"] = json.Number("12")
		}),
		"deleted with source": mutate(func(p map[string]any) { p["deleted"] = map[string]any{"source": "calls.deleted"} }),
		"recovery":            mutate(func(p map[string]any) { p["recovery"] = map[string]any{"relation": "absent-from-live", "notes": "x"} }),
		"snapshot":            mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "snap", "xid": int64(7)} }),
	}
	for name, p := range ok {
		if err := call.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCallDurationFractionalAndNegative: seconds as stored; fractions are fine,
// a negative or non-finite value is not, and a json.Number is read exactly.
func TestCallDurationFractionalAndNegative(t *testing.T) {
	with := func(v any) error {
		p := minimal().Payload()
		p["duration_s"] = v
		return call.Validate(p)
	}
	for name, v := range map[string]any{
		"0.5": 0.5, "zero": 0.0, "negative zero": math.Copysign(0, -1), "int": int64(30), "plain int": 30, "uint64": uint64(30), "large": 1e15,
		"json.Number 0.5": json.Number("0.5"), "json.Number 90": json.Number("90"), "json.Number 1e3": json.Number("1e3"), "json.Number 0": json.Number("0"),
	} {
		if err := with(v); err != nil {
			t.Errorf("duration %s refused: %v", name, err)
		}
	}
	for name, v := range map[string]any{
		"-1": float64(-1), "-0.5": -0.5, "tiny negative": -1e-300, "int -1": int64(-1), "NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
		"json.Number -1": json.Number("-1"), "json.Number -0.001": json.Number("-0.001"), "json.Number NaN": json.Number("NaN"),
		"json.Number Inf": json.Number("Inf"), "json.Number empty": json.Number(""), "json.Number text": json.Number("12s"), "json.Number overflow": json.Number("1e999"),
		"string": "0.5", "bool": false, "null": nil, "array": []any{1.0},
	} {
		if err := with(v); err == nil {
			t.Errorf("duration %s accepted", name)
		}
	}

	// the builder carries a fractional value exactly and a set zero
	for _, secs := range []float64{0, 0.5, 59.999, 3600} {
		c := minimal()
		c.DurationS = ptr(secs)
		if got := c.Payload()["duration_s"]; got != secs {
			t.Errorf("Payload duration_s = %v, want %v", got, secs)
		}
	}
	// a builder given a negative or NaN duration does not hide it: the validator refuses the payload
	for _, secs := range []float64{-1, math.NaN()} {
		c := minimal()
		c.DurationS = ptr(secs)
		if err := call.Validate(c.Payload()); err == nil {
			t.Errorf("a builder duration of %v validated", secs)
		}
	}

	// Decode reads fractions and exponents exactly and refuses a negative stored value
	for in, want := range map[string]float64{`0.5`: 0.5, `30`: 30, `1e3`: 1000, `0`: 0, `12.25`: 12.25} {
		c, err := call.Decode(1, []byte(`{"direction":"in","outcome":"answered","duration_s":`+in+`}`))
		if err != nil || c.DurationS == nil || *c.DurationS != want {
			t.Errorf("Decode(duration_s %s) = %+v, %v; want %v", in, c.DurationS, err, want)
		}
	}
	for _, in := range []string{`-1`, `-0.5`, `1e999`, `"1"`, `null`} {
		if _, err := call.Decode(1, []byte(`{"direction":"in","outcome":"answered","duration_s":`+in+`}`)); err == nil {
			t.Errorf("Decode accepted duration_s %s", in)
		}
	}
}

// TestCallBuilderOmitsAbsent (C3): an empty string and a nil pointer never
// appear; a set pointer does, even when it holds false or 0.
func TestCallBuilderOmitsAbsent(t *testing.T) {
	p := minimal().Payload()
	want := map[string]any{"direction": "unknown", "outcome": "unknown"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("minimal payload = %#v, want %#v", p, want)
	}

	p = hidden().Payload()
	want = map[string]any{
		"direction": "in", "outcome": "rejected", "address_presentation": "restricted",
		"duration_s": 0.0, "subscription": int64(0), "read": false, "new": false,
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("hidden payload = %#v, want %#v", p, want)
	}
	if _, has := p["address"]; has {
		t.Error("an absent address was written")
	}

	// every optional field is written exactly when it is set
	fp := full().Payload()
	for _, k := range []string{
		"direction", "outcome", "address", "address_presentation", "name_cached", "kind", "service", "country_iso", "geo_description",
		"duration_s", "subscription", "read", "new", "raw", "recovery", "snapshot", "deleted",
	} {
		if _, has := fp[k]; !has {
			t.Errorf("%s is missing from the full payload", k)
		}
	}
	if len(fp) != 17 {
		t.Errorf("full payload has %d keys, want 17: %v", len(fp), fp)
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

	// a call that holds nothing is an invalid payload: the omission is the builder's, the check the validator's
	if err := call.Validate(call.Call{}.Payload()); err == nil {
		t.Error("an empty call validated")
	}
}

// TestCallPayloadIsCanonicalTypes: only the types the records package
// canonicalizes, and the payload does not alias the Call.
func TestCallPayloadIsCanonicalTypes(t *testing.T) {
	for name, c := range map[string]call.Call{"minimal": minimal(), "full": full(), "hidden": hidden()} {
		if !canonical(c.Payload()) {
			t.Errorf("%s payload holds a type outside string, bool, int64, float64, []any, map[string]any", name)
		}
	}
	c := full()
	p := c.Payload()
	raw := rawOf(t, p)
	raw["type"] = "changed"
	raw["nested"].(map[string]any)["a"].([]any)[0] = "changed"
	rec, snap, del := objOf(t, p, "recovery"), objOf(t, p, "snapshot"), objOf(t, p, "deleted")
	rec["via"] = "changed"
	snap["name"] = "changed"
	del["source"] = "changed"
	if c.Recovery["via"] != "wal" || c.Snapshot["name"] != "com.apple.snap" || c.Deleted["source"] != "calls.deleted" {
		t.Error("the payload aliases the call's recovery, snapshot or deleted map")
	}
	if c.Raw["type"] != int64(3) || c.Raw["nested"].(map[string]any)["a"].([]any)[0] != "x" {
		t.Error("the payload aliases the call's raw map")
	}
	// the duration is copied, not pointed to
	c.DurationS = ptr(7.0)
	p = c.Payload()
	*c.DurationS = 9
	if p["duration_s"] != 7.0 {
		t.Errorf("duration_s followed a later change of the pointer: %v", p["duration_s"])
	}
	// two calls give independent maps
	a, b := full().Payload(), full().Payload()
	rawOf(t, a)["type"] = "changed"
	if rawOf(t, b)["type"] == "changed" {
		t.Error("payloads share state")
	}
}

// rawOf returns the raw object of a payload, failing the test when there is none.
func rawOf(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	raw, ok := p["raw"].(map[string]any)
	if !ok {
		t.Fatalf("the payload has no raw object: %v", p)
	}
	return raw
}

func canonical(v any) bool {
	switch x := v.(type) {
	case string, bool, int64, float64:
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
	cases := []struct{ dir, outcome, addr, want string }{
		{"in", "missed", "+15551234567", "call in missed +15551234567"},
		{"out", "answered", "me@example.com", "call out answered me@example.com"},
		{"in", "rejected", "", "call in rejected"},
		{"unknown", "unknown", "a\nb\tc", "call unknown unknown a b c"},
	}
	for _, tc := range cases {
		if got := call.Summary(tc.dir, tc.outcome, tc.addr); got != tc.want {
			t.Errorf("Summary(%q,%q,%q) = %q, want %q", tc.dir, tc.outcome, tc.addr, got, tc.want)
		}
	}
	// hostile parts stay one line, show bidi and format characters as escapes, and are bounded
	rlo, zwj, bom, zwsp := string(rune(0x202E)), string(rune(0x200D)), string(rune(0xFEFF)), string(rune(0x200B))
	h := call.Summary(strings.Repeat("c", 1000)+"\n", "out"+rlo, strings.Repeat("9", 1000)+"\x00"+zwj+bom)
	if strings.ContainsAny(h, "\n\x00"+rlo+zwj+bom) || len(h) > len("call ")+16+1+16+1+64 {
		t.Errorf("hostile summary = %q (%d bytes)", h, len(h))
	}
	if spoof := call.Summary("in", "missed", rlo+"+15550000000"+bom); spoof != "call in missed <U+202E>+15550000000<U+FEFF>" {
		t.Errorf("a bidi override is not visible in the summary: %q", spoof)
	}
	if call.Summary("in", "missed", "+15550000000") == call.Summary("in", "missed", "+1555"+zwsp+"0000000") {
		t.Error("two different addresses give the same summary")
	}
}

// FuzzCallValidate: arbitrary JSON into Validate never panics, and an accepted
// payload always decodes (and decodes to something that validates again).
func FuzzCallValidate(f *testing.F) {
	for _, c := range []call.Call{minimal(), full(), hidden()} {
		b, err := json.Marshal(c.Payload())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","duration_s":-0.0}`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","duration_s":1e999}`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","duration_s":1e-400,"raw":{"a":{"b":{"c":{}}}}}`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","subscription":9223372036854775808}`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","subscription":1e3,"duration_s":1e3}`))
	f.Add([]byte(`{"direction":"in","outcome":"missed","recovery":{"relation":"uncommitted"},"snapshot":{"name":"s","xid":2.0},"deleted":{"source":"x"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return
		}
		if err := call.Validate(m); err != nil {
			if len(err.Error()) > 1<<12 {
				t.Errorf("error too long: %d bytes", len(err.Error()))
			}
			return
		}
		got, err := call.Decode(call.PayloadVersion, data)
		if err != nil {
			// only trailing data after the first value can make a validated object fail to decode
			if len(strings.TrimSpace(string(data[dec.InputOffset():]))) == 0 {
				t.Fatalf("an accepted payload does not decode: %v", err)
			}
			return
		}
		again, err := call.Decode(call.PayloadVersion, data)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatalf("Decode is not deterministic: %v", err)
		}
		if got.DurationS != nil && (*got.DurationS < 0 || math.IsNaN(*got.DurationS) || math.IsInf(*got.DurationS, 0)) {
			t.Fatalf("a validated call decoded to a duration of %v", *got.DurationS)
		}
		if err := call.Validate(got.Payload()); err != nil {
			t.Fatalf("the decoded call does not produce a valid payload: %v", err)
		}
	})
}

// v1Fixture is a stored v1 payload as the writer canonicalizes it, written out by
// hand so it does not move when the builder changes. It carries a field a future
// version might add (future_field).
const v1Fixture = `{
  "address": "+15551234567",
  "address_presentation": "allowed",
  "country_iso": "US",
  "direction": "in",
  "duration_s": 12.5,
  "future_field": {"anything": [1, 2, 3]},
  "geo_description": "Oregon",
  "kind": "voice",
  "name_cached": "Alex",
  "new": true,
  "outcome": "answered",
  "raw": {"type": 1, "features": 0, "ratio": 0.5, "tags": ["a", "b"], "nested": {"ok": true}},
  "deleted": {"source": "calls.deleted"},
  "read": false,
  "recovery": {"relation": "absent-from-live", "via": "wal", "wal": {"frame": 3, "committed": true}},
  "service": "cellular",
  "snapshot": {"name": "snap-1", "xid": 12},
  "subscription": 2
}`

func TestRecordTypesDecodeOlderVersions(t *testing.T) {
	t.Run("the version constant is the registered payload version", func(t *testing.T) {
		ty, ok := records.LookupType(call.Type)
		if !ok {
			t.Fatal("call is not a registered type")
		}
		if ty.PayloadVersion != call.PayloadVersion {
			t.Errorf("registered payload version %d, package constant %d", ty.PayloadVersion, call.PayloadVersion)
		}
	})

	t.Run("round trip equals the input", func(t *testing.T) {
		for name, c := range map[string]call.Call{"minimal": minimal(), "full": full(), "hidden": hidden()} {
			b, err := json.Marshal(c.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := call.Decode(call.PayloadVersion, b)
			if err != nil {
				t.Fatalf("%s: Decode = %v", name, err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Errorf("%s: round trip\n got  %+v\n want %+v", name, got, c)
			}
			if !reflect.DeepEqual(got.Payload(), c.Payload()) {
				t.Errorf("%s: the decoded call builds another payload", name)
			}
		}
	})

	t.Run("v1 fixture bytes decode", func(t *testing.T) {
		got, err := call.Decode(1, []byte(v1Fixture))
		if err != nil {
			t.Fatalf("Decode(v1 fixture) = %v", err)
		}
		want := call.Call{
			Direction: "in", Outcome: "answered", Address: "+15551234567", AddressPresentation: "allowed", NameCached: "Alex",
			Kind: "voice", Service: "cellular", CountryISO: "US", GeoDescription: "Oregon",
			DurationS: ptr(12.5), Subscription: ptr(int64(2)), Read: ptr(false), New: ptr(true),
			Raw:      map[string]any{"type": int64(1), "features": int64(0), "ratio": 0.5, "tags": []any{"a", "b"}, "nested": map[string]any{"ok": true}},
			Deleted:  map[string]any{"source": "calls.deleted"},
			Recovery: map[string]any{"relation": "absent-from-live", "via": "wal", "wal": map[string]any{"frame": int64(3), "committed": true}},
			Snapshot: map[string]any{"name": "snap-1", "xid": int64(12)},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Decode(v1 fixture)\n got  %+v\n want %+v", got, want)
		}
		var p map[string]any
		dec := json.NewDecoder(strings.NewReader(v1Fixture))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if err := call.Validate(p); err != nil {
			t.Errorf("Validate(v1 fixture) = %v", err)
		}
	})

	t.Run("a future-added unknown field is ignored", func(t *testing.T) {
		got, err := call.Decode(1, []byte(`{"direction":"out","outcome":"cancelled","new_in_v1_1":{"deep":[1,{"x":null}]},"another":"x"}`))
		if err != nil {
			t.Fatalf("Decode = %v", err)
		}
		if got.Direction != "out" || got.Outcome != "cancelled" || got.Address != "" || got.DurationS != nil || got.Raw != nil {
			t.Errorf("Decode = %+v", got)
		}
	})

	t.Run("other payload versions are unsupported", func(t *testing.T) {
		for _, v := range []int{0, 2, 3, -1, 1 << 30} {
			_, err := call.Decode(v, []byte(v1Fixture))
			if !errors.Is(err, call.ErrUnsupportedPayloadVersion) {
				t.Errorf("Decode(%d) = %v, want ErrUnsupportedPayloadVersion", v, err)
			}
		}
		if _, err := call.Decode(1, []byte(v1Fixture)); errors.Is(err, call.ErrUnsupportedPayloadVersion) {
			t.Error("v1 reported as unsupported")
		}
	})

	t.Run("damaged payloads are errors, never a panic and never an unsupported version", func(t *testing.T) {
		const marker = "SECRET-VALUE-9931"
		for name, b := range map[string]string{
			"empty":              ``,
			"not json":           `{"direction": `,
			"null":               `null`,
			"array":              `[]`,
			"string":             `"x"`,
			"trailing value":     `{"direction":"in","outcome":"missed"} {}`,
			"trailing garbage":   `{"direction":"in","outcome":"missed"} x`,
			"missing direction":  `{"outcome":"missed"}`,
			"missing outcome":    `{"direction":"in"}`,
			"wrong type":         `{"direction":"in","outcome":"missed","read":"` + marker + `"}`,
			"enum":               `{"direction":"` + marker + `","outcome":"missed"}`,
			"big exponent":       `{"direction":"in","outcome":"missed","subscription":1e999}`,
			"fractional integer": `{"direction":"in","outcome":"missed","subscription":1.5}`,
			"exponent integer":   `{"direction":"in","outcome":"missed","subscription":1e3}`,
			"decimal integer":    `{"direction":"in","outcome":"missed","subscription":2.0}`,
		} {
			_, err := call.Decode(1, []byte(b))
			if err == nil {
				t.Errorf("%s: no error", name)
				continue
			}
			if errors.Is(err, call.ErrUnsupportedPayloadVersion) {
				t.Errorf("%s: reported as an unsupported version", name)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s: error repeats a payload value: %v", name, err)
			}
		}
	})

	t.Run("deeply nested input does not panic", func(t *testing.T) {
		deep := strings.Repeat(`{"a":`, 2000) + `1` + strings.Repeat(`}`, 2000)
		if _, err := call.Decode(1, []byte(`{"direction":"in","outcome":"missed","raw":`+deep+`}`)); err == nil {
			t.Error("a payload nested 2000 deep was accepted")
		}
	})
}

// objOf returns the object under key in a payload, failing the test when there is none.
func objOf(t *testing.T, p map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := p[key].(map[string]any)
	if !ok {
		t.Fatalf("the payload has no %s object: %v", key, p)
	}
	return o
}

// TestDecodeKeepsRecoveryAndSnapshot: a typed reader can never present a recovered
// or snapshot-derived call as a live one, so Decode carries the provenance objects
// (and the deleted marker) and Payload writes them back.
func TestDecodeKeepsRecoveryAndSnapshot(t *testing.T) {
	const in = `{"direction":"in","outcome":"missed",` +
		`"recovery":{"relation":"uncommitted","via":"wal","wal":{"frame":3,"salt1":7,"committed":false},"notes":["a","b"]},` +
		`"snapshot":{"name":"snap-1","xid":12},"deleted":{"source":"calls.deleted"}}`
	got, err := call.Decode(1, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	wantRec := map[string]any{
		"relation": "uncommitted", "via": "wal", "notes": []any{"a", "b"},
		"wal": map[string]any{"frame": int64(3), "salt1": int64(7), "committed": false},
	}
	if !reflect.DeepEqual(got.Recovery, wantRec) {
		t.Errorf("Recovery = %#v, want %#v", got.Recovery, wantRec)
	}
	if want := map[string]any{"name": "snap-1", "xid": int64(12)}; !reflect.DeepEqual(got.Snapshot, want) {
		t.Errorf("Snapshot = %#v, want %#v", got.Snapshot, want)
	}
	if want := map[string]any{"source": "calls.deleted"}; !reflect.DeepEqual(got.Deleted, want) {
		t.Errorf("Deleted = %#v, want %#v", got.Deleted, want)
	}
	p := got.Payload()
	if !reflect.DeepEqual(p["recovery"], wantRec) || !reflect.DeepEqual(p["snapshot"], map[string]any{"name": "snap-1", "xid": int64(12)}) ||
		!reflect.DeepEqual(p["deleted"], map[string]any{"source": "calls.deleted"}) {
		t.Errorf("Payload dropped provenance: %v", p)
	}
	if err := call.Validate(p); err != nil {
		t.Errorf("the re-built payload does not validate: %v", err)
	}
	live, err := call.Decode(1, []byte(`{"direction":"in","outcome":"missed"}`))
	if err != nil || live.Recovery != nil || live.Snapshot != nil || live.Deleted != nil {
		t.Errorf("a live call decoded with provenance: %+v, %v", live, err)
	}
	for _, k := range []string{"recovery", "snapshot", "deleted"} {
		if _, has := live.Payload()[k]; has {
			t.Errorf("a live call built a %s key", k)
		}
	}
}
