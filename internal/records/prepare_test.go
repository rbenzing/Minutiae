package records_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

func ptr[T any](v T) *T { return &v }

var testArt = records.ArtifactInfo{ID: "art-1", SHA256: strings.Repeat("a", 64), Size: 1000}

func base() records.Record {
	return records.Record{Type: "message", ArtifactID: "art-1", Payload: map[string]any{"k": "v"}}
}

func utc(sec int64) *records.Time { return &records.Time{T: time.Unix(sec, 0).UTC()} }

func localOffset(sec int64, off int) records.Time {
	return records.Time{T: time.Unix(sec, 0).UTC(), Basis: records.BasisLocalOffset, OffsetMin: off}
}

func TestPrepareValidationMatrix(t *testing.T) {
	big := func(n int) string { return strings.Repeat("x", n) }
	// a payload whose canonical encoding is exactly n bytes: {"k":"<n-8 x>"}
	payloadOfSize := func(n int) map[string]any { return map[string]any{"k": big(n - 8)} }
	kinds := func(n int) []records.NamedTime {
		var out []records.NamedTime
		for i := 0; i < n; i++ {
			out = append(out, records.NamedTime{Kind: "kind_" + string(rune('a'+i)), Time: *utc(int64(i))})
		}
		return out
	}
	nested := func(depth int) map[string]any { // a container nesting of exactly depth levels, the top one included
		m := map[string]any{}
		cur := m
		for i := 1; i < depth; i++ {
			next := map[string]any{}
			cur["n"] = next
			cur = next
		}
		return m
	}
	type tc struct {
		name   string
		mutate func(r *records.Record)
		want   error // nil = accepted
	}
	lo := func(r *records.Record, off int) {
		t := localOffset(5, off)
		r.Time = &t
	}
	cases := []tc{
		{name: "base is valid"},
		{name: "unregistered type", mutate: func(r *records.Record) { r.Type = "no_such_type" }, want: records.ErrUnknownType},
		{name: "empty type", mutate: func(r *records.Record) { r.Type = "" }, want: records.ErrUnknownType},
		{name: "empty artifact", mutate: func(r *records.Record) { r.ArtifactID = "" }, want: records.ErrUnknownArtifact},
		{name: "unknown artifact", mutate: func(r *records.Record) { r.ArtifactID = "other" }, want: records.ErrUnknownArtifact},

		{name: "range at the artifact size", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 900, Length: 100} }},
		{name: "range one byte beyond", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 900, Length: 101} }, want: records.ErrInvalidRange},
		{name: "empty range at the end", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 1000, Length: 0} }},
		{name: "offset beyond the end", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 1001, Length: 0} }, want: records.ErrInvalidRange},
		{name: "zero range is a real claim and in bounds", mutate: func(r *records.Record) { r.Range = &records.Range{} }},
		{name: "negative offset", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: -1, Length: 1} }, want: records.ErrInvalidRange},
		{name: "negative length", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 0, Length: -1} }, want: records.ErrInvalidRange},
		{name: "offset+length overflows", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: math.MaxInt64, Length: 1} }, want: records.ErrInvalidRange},
		{name: "offset+length overflows (both large)", mutate: func(r *records.Record) {
			r.Range = &records.Range{Offset: math.MaxInt64/2 + 1, Length: math.MaxInt64/2 + 1}
		}, want: records.ErrInvalidRange},
		{name: "max length from offset 0 beyond the artifact", mutate: func(r *records.Record) { r.Range = &records.Range{Offset: 0, Length: math.MaxInt64} }, want: records.ErrInvalidRange},

		{name: "summary with a newline", mutate: func(r *records.Record) { r.Summary = "line one\nline two\ttab" }},
		{name: "empty summary", mutate: func(r *records.Record) { r.Summary = "" }},
		{name: "NUL in summary", mutate: func(r *records.Record) { r.Summary = "a\x00b" }, want: records.ErrInvalidText},
		{name: "invalid UTF-8 in summary", mutate: func(r *records.Record) { r.Summary = "a\xffb" }, want: records.ErrInvalidText},
		{name: "NUL in body", mutate: func(r *records.Record) { r.Body = "a\x00b" }, want: records.ErrInvalidText},
		{name: "invalid UTF-8 in body", mutate: func(r *records.Record) { r.Body = "\xc3\x28" }, want: records.ErrInvalidText},
		{name: "NUL in source path", mutate: func(r *records.Record) { r.SourcePath = "a\x00b" }, want: records.ErrInvalidText},
		{name: "invalid UTF-8 in source path", mutate: func(r *records.Record) { r.SourcePath = "a\xffb" }, want: records.ErrInvalidText},
		{name: "NUL in locator", mutate: func(r *records.Record) { r.Locator = "sqlite:t=a\x00" }, want: records.ErrInvalidText},
		{name: "invalid UTF-8 in locator", mutate: func(r *records.Record) { r.Locator = "sqlite:t=\xff" }, want: records.ErrInvalidText},
		{name: "NUL in a payload string", mutate: func(r *records.Record) { r.Payload = map[string]any{"k": "a\x00"} }, want: records.ErrInvalidPayload},
		{name: "invalid UTF-8 in a payload string", mutate: func(r *records.Record) { r.Payload = map[string]any{"k": "a\xff"} }, want: records.ErrInvalidPayload},
		{name: "invalid UTF-8 in a payload key", mutate: func(r *records.Record) { r.Payload = map[string]any{"a\xff": 1} }, want: records.ErrInvalidPayload},
		{name: "invalid UTF-8 in a nested payload string", mutate: func(r *records.Record) { r.Payload = map[string]any{"k": []any{map[string]any{"z": "\xfe"}}} }, want: records.ErrInvalidPayload},

		{name: "summary at the cap", mutate: func(r *records.Record) { r.Summary = big(records.MaxSummary) }},
		{name: "summary over the cap", mutate: func(r *records.Record) { r.Summary = big(records.MaxSummary + 1) }, want: records.ErrRecordTooLarge},
		{name: "summary cap counts bytes", mutate: func(r *records.Record) { r.Summary = strings.Repeat("é", records.MaxSummary/2+1) }, want: records.ErrRecordTooLarge},
		{name: "body at the cap", mutate: func(r *records.Record) { r.Body = big(records.MaxBody) }},
		{name: "body over the cap", mutate: func(r *records.Record) { r.Body = big(records.MaxBody + 1) }, want: records.ErrRecordTooLarge},
		{name: "source path at the cap", mutate: func(r *records.Record) { r.SourcePath = big(records.MaxSourcePath) }},
		{name: "source path over the cap", mutate: func(r *records.Record) { r.SourcePath = big(records.MaxSourcePath + 1) }, want: records.ErrRecordTooLarge},
		{name: "locator at the cap", mutate: func(r *records.Record) { r.Locator = "sqlite:" + big(records.MaxLocator-7) }},
		{name: "locator over the cap", mutate: func(r *records.Record) { r.Locator = "sqlite:" + big(records.MaxLocator-6) }, want: records.ErrRecordTooLarge},
		{name: "payload at the cap", mutate: func(r *records.Record) { r.Payload = payloadOfSize(records.MaxPayload) }},
		{name: "payload over the cap", mutate: func(r *records.Record) { r.Payload = payloadOfSize(records.MaxPayload + 1) }, want: records.ErrRecordTooLarge},
		{name: "payload depth at the cap", mutate: func(r *records.Record) { r.Payload = nested(records.MaxPayloadDepth) }},
		{name: "payload depth over the cap", mutate: func(r *records.Record) { r.Payload = nested(records.MaxPayloadDepth + 1) }, want: records.ErrInvalidPayload},
		{name: "16 times", mutate: func(r *records.Record) { r.Time = utc(1); r.Times = kinds(records.MaxTimes) }},
		{name: "17 times", mutate: func(r *records.Record) { r.Time = utc(1); r.Times = kinds(records.MaxTimes + 1) }, want: records.ErrRecordTooLarge},

		{name: "recovery without deleted", mutate: func(r *records.Record) { r.Recovery = "carve" }, want: records.ErrInvalidField},
		{name: "recovery with deleted", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "carve" }},
		{name: "recovery with a dash", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "free-page" }},
		{name: "deleted alone", mutate: func(r *records.Record) { r.Deleted = true }},
		{name: "recovery token upper case", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "Carve" }, want: records.ErrInvalidField},
		{name: "recovery token one character", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "c" }, want: records.ErrInvalidField},
		{name: "recovery token with a space", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "free page" }, want: records.ErrInvalidField},
		{name: "recovery token starting with a dash", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "-ab" }, want: records.ErrInvalidField},
		{name: "recovery token too long", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "a" + big(32) }, want: records.ErrInvalidField},
		{name: "recovery token at 32", mutate: func(r *records.Record) { r.Deleted = true; r.Recovery = "a" + big(31) }},

		{name: "confidence 0", mutate: func(r *records.Record) { r.Confidence = ptr(0) }},
		{name: "confidence 100", mutate: func(r *records.Record) { r.Confidence = ptr(100) }},
		{name: "confidence 101", mutate: func(r *records.Record) { r.Confidence = ptr(101) }, want: records.ErrInvalidField},
		{name: "confidence -1", mutate: func(r *records.Record) { r.Confidence = ptr(-1) }, want: records.ErrInvalidField},

		{name: "time year 300000 overflows", mutate: func(r *records.Record) {
			r.Time = &records.Time{T: time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC)}
			r.Payload = map[string]any{"ts_note": "raw"}
		}, want: records.ErrInvalidTime},
		{name: "time year -300000 overflows", mutate: func(r *records.Record) {
			r.Time = &records.Time{T: time.Date(-300000, 1, 1, 0, 0, 0, 0, time.UTC)}
			r.Payload = map[string]any{"ts_note": "raw"}
		}, want: records.ErrInvalidTime},
		{name: "time before 1970 without a note", mutate: func(r *records.Record) { r.Time = utc(-1) }, want: records.ErrInvalidTime},
		{name: "time before 1970 with a note", mutate: func(r *records.Record) { r.Time = utc(-1); r.Payload = map[string]any{"ts_note": "device clock unset"} }},
		{name: "time before 1970 with an empty note", mutate: func(r *records.Record) { r.Time = utc(-1); r.Payload = map[string]any{"ts_note": ""} }, want: records.ErrInvalidTime},
		{name: "time before 1970 with a non-string note", mutate: func(r *records.Record) { r.Time = utc(-1); r.Payload = map[string]any{"ts_note": 5} }, want: records.ErrInvalidTime},
		{name: "time at the 2100 limit without a note", mutate: func(r *records.Record) { r.Time = utc(4102444800) }, want: records.ErrInvalidTime},
		{name: "time just below the 2100 limit", mutate: func(r *records.Record) { r.Time = utc(4102444799) }},
		{name: "time at 1970", mutate: func(r *records.Record) { r.Time = utc(0) }},
		{name: "end time out of range without a note", mutate: func(r *records.Record) { r.Time = utc(5); r.TimeEnd = utc(-5) }, want: records.ErrInvalidTime},
		{name: "end time without a start", mutate: func(r *records.Record) { r.TimeEnd = utc(5) }, want: records.ErrInvalidTime},
		{name: "end time with another basis than the start", mutate: func(r *records.Record) {
			r.Time = utc(5)
			e := localOffset(9, 60)
			r.TimeEnd = &e
		}, want: records.ErrInvalidTime},
		{name: "end time with another offset than the start", mutate: func(r *records.Record) {
			s, e := localOffset(5, 60), localOffset(9, 120)
			r.Time, r.TimeEnd = &s, &e
		}, want: records.ErrInvalidTime},
		{name: "end time with the start's basis and offset", mutate: func(r *records.Record) {
			s, e := localOffset(5, 60), localOffset(9, 60)
			r.Time, r.TimeEnd = &s, &e
		}},
		{name: "named time out of range without a note", mutate: func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "read", Time: *utc(-5)}}
		}, want: records.ErrInvalidTime},
		{name: "unknown basis", mutate: func(r *records.Record) { r.Time = &records.Time{T: time.Unix(5, 0), Basis: "gps"} }, want: records.ErrInvalidTime},
		{name: "offset with utc basis", mutate: func(r *records.Record) {
			r.Time = &records.Time{T: time.Unix(5, 0), Basis: records.BasisUTC, OffsetMin: 60}
		}, want: records.ErrInvalidTime},
		{name: "offset with default basis", mutate: func(r *records.Record) { r.Time = &records.Time{T: time.Unix(5, 0), OffsetMin: 60} }, want: records.ErrInvalidTime},
		{name: "offset with local-unknown", mutate: func(r *records.Record) {
			r.Time = &records.Time{T: time.Unix(5, 0), Basis: records.BasisLocalUnknown, OffsetMin: 60}
		}, want: records.ErrInvalidTime},
		{name: "local-offset with 0", mutate: func(r *records.Record) { lo(r, 0) }},
		{name: "local-offset 1439", mutate: func(r *records.Record) { lo(r, 1439) }},
		{name: "local-offset -1439", mutate: func(r *records.Record) { lo(r, -1439) }},
		{name: "local-offset 1440", mutate: func(r *records.Record) { lo(r, 1440) }, want: records.ErrInvalidTime},
		{name: "local-offset -1440", mutate: func(r *records.Record) { lo(r, -1440) }, want: records.ErrInvalidTime},
		{name: "bad offset on a named time", mutate: func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "read", Time: localOffset(5, 2000)}}
		}, want: records.ErrInvalidTime},

		{name: "kind one character", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "a", Time: *utc(1)}} }, want: records.ErrInvalidField},
		{name: "kind upper case", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "Read", Time: *utc(1)}} }, want: records.ErrInvalidField},
		{name: "kind with a dash", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "re-ad", Time: *utc(1)}} }, want: records.ErrInvalidField},
		{name: "kind empty", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "", Time: *utc(1)}} }, want: records.ErrInvalidField},
		{name: "kind 33 characters", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "a" + big(32), Time: *utc(1)}} }, want: records.ErrInvalidField},
		{name: "kind 32 characters", mutate: func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "a" + big(31), Time: *utc(1)}} }},
		{name: "duplicate kind", mutate: func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "read", Time: *utc(1)}, {Kind: "read", Time: *utc(2)}}
		}, want: records.ErrInvalidField},

		{name: "locator scheme", mutate: func(r *records.Record) { r.Locator = "sqlite:table=sms;rowid=1" }},
		{name: "locator scheme upper case", mutate: func(r *records.Record) { r.Locator = "SQLite:table=sms" }, want: records.ErrInvalidLocator},
		{name: "locator without a scheme", mutate: func(r *records.Record) { r.Locator = "table=sms" }, want: records.ErrInvalidLocator},
		{name: "locator scheme one character", mutate: func(r *records.Record) { r.Locator = "s:x=1" }, want: records.ErrInvalidLocator},
		{name: "locator scheme 17 characters", mutate: func(r *records.Record) { r.Locator = "a" + big(16) + ":x=1" }, want: records.ErrInvalidLocator},
		{name: "locator scheme 16 characters", mutate: func(r *records.Record) { r.Locator = "a" + big(15) + ":x=1" }},
		{name: "locator scheme with a dash", mutate: func(r *records.Record) { r.Locator = "my-scheme:x=1" }, want: records.ErrInvalidLocator},
		{name: "locator scheme starting with a digit", mutate: func(r *records.Record) { r.Locator = "1ab:x=1" }, want: records.ErrInvalidLocator},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base()
			if c.mutate != nil {
				c.mutate(&r)
			}
			_, err := records.Prepare(r, testArt)
			if c.want == nil {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if !errors.Is(err, records.ErrInvalidRecord) {
				t.Fatalf("err = %v does not wrap ErrInvalidRecord", err)
			}
		})
	}

	t.Run("unknown artifact wraps evidence.ErrUnknownArtifact", func(t *testing.T) {
		r := base()
		r.ArtifactID = "other"
		if _, err := records.Prepare(r, testArt); !errors.Is(err, evidence.ErrUnknownArtifact) {
			t.Fatalf("err = %v, want evidence.ErrUnknownArtifact", err)
		}
	})
}

func TestPrepareFillsRow(t *testing.T) {
	t.Run("minimal record: everything optional is NULL", func(t *testing.T) {
		p, err := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1", Payload: nil}, testArt)
		if err != nil {
			t.Fatal(err)
		}
		r := p.Row()
		if r.ID != 0 || r.Type != "event" || r.PayloadV != 1 || r.ArtifactID != "art-1" {
			t.Errorf("row = %+v", r)
		}
		if r.SourcePath != nil || r.Locator != nil || r.SrcOffset != nil || r.SrcLength != nil ||
			r.TS != nil || r.TSEnd != nil || r.TSBasis != nil || r.TZOffsetMin != nil ||
			r.RecoveryMethod != nil || r.Confidence != nil || r.Body != nil {
			t.Errorf("an unset optional field is not NULL: %+v", r)
		}
		if r.Deleted || r.Recovered || len(r.Times) != 0 || r.Summary != "" || r.Payload != "{}" {
			t.Errorf("row = %+v", r)
		}
		if r.ParserName != "" || r.ParserVersion != "" || r.ParserHash != nil {
			t.Errorf("parser fields are the writer's: %+v", r)
		}
		if p.ApproxBytes() <= 0 {
			t.Errorf("approxBytes = %d", p.ApproxBytes())
		}
	})

	t.Run("empty strings are NULL, not empty", func(t *testing.T) {
		p, err := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1", SourcePath: "", Locator: "", Body: "", Summary: ""}, testArt)
		if err != nil {
			t.Fatal(err)
		}
		if r := p.Row(); r.SourcePath != nil || r.Locator != nil || r.Body != nil {
			t.Errorf("source path/locator/body = %v %v %v, want NULL", r.SourcePath, r.Locator, r.Body)
		}
	})

	t.Run("full record", func(t *testing.T) {
		start, end := records.Time{T: time.Unix(1700000000, 123456789).UTC()}, records.Time{T: time.Unix(1700000060, 0).UTC()}
		rec := records.Record{
			Type: "message", ArtifactID: "art-1",
			SourcePath: "/data/x.db", Locator: "sqlite:table=sms;rowid=7",
			Range: &records.Range{Offset: 10, Length: 20},
			Time:  &start, TimeEnd: &end,
			Times: []records.NamedTime{
				{Kind: "read", Time: records.Time{T: time.Unix(1700000100, 0).UTC(), Basis: records.BasisLocalUnknown}},
				{Kind: "delivered", Time: localOffset(1700000050, 60)},
			},
			Deleted: true, Recovery: "free-page", Confidence: ptr(0),
			Summary: "hello\nworld", Body: "body", Payload: map[string]any{"b": 1, "a": "x"},
		}
		p, err := records.Prepare(rec, testArt)
		if err != nil {
			t.Fatal(err)
		}
		r := p.Row()
		if r.SourcePath == nil || *r.SourcePath != "/data/x.db" || r.Locator == nil || *r.Locator != "sqlite:table=sms;rowid=7" {
			t.Errorf("path/locator = %v %v", r.SourcePath, r.Locator)
		}
		if r.SrcOffset == nil || *r.SrcOffset != 10 || r.SrcLength == nil || *r.SrcLength != 20 {
			t.Errorf("range = %v %v", r.SrcOffset, r.SrcLength)
		}
		if r.TS == nil || *r.TS != 1700000000123456 { // microseconds, sub-microsecond dropped
			t.Errorf("ts = %v", r.TS)
		}
		if r.TSBasis == nil || *r.TSBasis != "utc" || r.TZOffsetMin != nil {
			t.Errorf("basis = %v tz = %v, want utc and NULL (basis defaults to utc)", r.TSBasis, r.TZOffsetMin)
		}
		if r.TSEnd == nil || *r.TSEnd != 1700000060000000 {
			t.Errorf("ts_end = %v", r.TSEnd)
		}
		if !r.Deleted || !r.Recovered || r.RecoveryMethod == nil || *r.RecoveryMethod != "free-page" {
			t.Errorf("deleted/recovered/method = %v %v %v", r.Deleted, r.Recovered, r.RecoveryMethod)
		}
		if r.Confidence == nil || *r.Confidence != 0 {
			t.Errorf("confidence = %v, want a stated 0", r.Confidence)
		}
		if r.Summary != "hello\nworld" || r.Body == nil || *r.Body != "body" {
			t.Errorf("summary/body = %q %v", r.Summary, r.Body)
		}
		if r.Payload != `{"a":"x","b":1}` {
			t.Errorf("payload = %s", r.Payload)
		}
		if len(r.Times) != 2 || r.Times[0].Kind != "delivered" || r.Times[1].Kind != "read" {
			t.Fatalf("times = %+v, want sorted by kind", r.Times)
		}
		d := r.Times[0]
		if d.TS != 1700000050000000 || d.Basis != "local-offset" || d.TZOffsetMin == nil || *d.TZOffsetMin != 60 {
			t.Errorf("delivered = %+v", d)
		}
		rd := r.Times[1]
		if rd.Basis != "local-unknown" || rd.TZOffsetMin != nil {
			t.Errorf("read = %+v", rd)
		}
	})

	t.Run("local-offset start carries its offset", func(t *testing.T) {
		s := localOffset(5, -330)
		p, err := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1", Time: &s}, testArt)
		if err != nil {
			t.Fatal(err)
		}
		r := p.Row()
		if r.TSBasis == nil || *r.TSBasis != "local-offset" || r.TZOffsetMin == nil || *r.TZOffsetMin != -330 {
			t.Errorf("basis = %v tz = %v", r.TSBasis, r.TZOffsetMin)
		}
	})

	t.Run("a stated confidence 0 differs from none", func(t *testing.T) {
		a, err1 := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1"}, testArt)
		b, err2 := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1", Confidence: ptr(0)}, testArt)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if a.Row().Confidence != nil || b.Row().Confidence == nil {
			t.Errorf("confidence = %v / %v", a.Row().Confidence, b.Row().Confidence)
		}
	})

	t.Run("the caller's record is not modified", func(t *testing.T) {
		times := []records.NamedTime{{Kind: "zeta", Time: *utc(1)}, {Kind: "alpha", Time: *utc(2)}}
		if _, err := records.Prepare(records.Record{Type: "event", ArtifactID: "art-1", Time: utc(1), Times: times}, testArt); err != nil {
			t.Fatal(err)
		}
		if times[0].Kind != "zeta" || times[1].Kind != "alpha" {
			t.Errorf("Prepare reordered the caller's Times: %+v", times)
		}
	})
}
