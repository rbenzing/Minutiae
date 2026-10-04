package records_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

func TestCanonicalPayloadSortedKeysNumbersExact(t *testing.T) {
	p := map[string]any{
		"zeta":  1,
		"alpha": map[string]any{"y": []any{1, "two", nil, true, false}, "x": map[string]any{}},
		"Beta":  "upper case keys sort before lower case (byte order)",
		"nums": []any{
			json.Number("12345678901234567890"), // beyond int64 and float64: kept as written
			json.Number("1.50"),                 // not normalised
			json.Number("-0"),
			json.Number("1E+3"),
			int(-7), int64(math.MinInt64), uint64(math.MaxUint64),
			float64(1.5), float64(-0.25), float64(100),
		},
		"html":    `<a href="x">&amp;</a>`,
		"escapes": "q\" b\\ n\n r\r t\t c\x01 d\x1f del\x7f",
		"unicode": "é€😀  ",
		"empty":   "",
		"nilmap":  map[string]any(nil),
		"nilarr":  []any(nil),
		"nothing": nil,
	}
	want := `{"Beta":"upper case keys sort before lower case (byte order)",` +
		`"alpha":{"x":{},"y":[1,"two",null,true,false]},` +
		`"empty":"",` +
		`"escapes":"q\" b\\ n\n r\r t\t c\u0001 d\u001f del` + "\x7f" + `",` +
		`"html":"<a href=\"x\">&amp;</a>",` +
		`"nilarr":[],"nilmap":{},"nothing":null,` +
		`"nums":[12345678901234567890,1.50,-0,1E+3,-7,-9223372036854775808,18446744073709551615,1.5,-0.25,100],` +
		"\"unicode\":\"é€😀  \"," +
		`"zeta":1}`
	got, err := records.CanonicalPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("canonical payload:\n got %s\nwant %s", got, want)
	}
	// deterministic
	for i := 0; i < 20; i++ {
		again, err := records.CanonicalPayload(p)
		if err != nil || again != got {
			t.Fatalf("run %d: %q, %v", i, again, err)
		}
	}
	// a nil map is an empty object
	if got, err := records.CanonicalPayload(nil); err != nil || got != "{}" {
		t.Errorf("nil payload = %q, %v", got, err)
	}
	// the output is valid JSON that decodes to the same values
	var back map[string]any
	dec := json.NewDecoder(strings.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	again, err := records.CanonicalPayload(back)
	if err != nil || again != got {
		t.Errorf("canonical form is not a fixed point:\n%s\n%s (%v)", got, again, err)
	}
}

func TestCanonicalPayloadRejectsHostile(t *testing.T) {
	type custom struct{ A int }
	deep := func(n int) any {
		var v any = "leaf"
		for i := 0; i < n; i++ {
			v = []any{v}
		}
		return v
	}
	cases := []struct {
		name string
		p    any
		want string // substring of the error
	}{
		{"NaN", map[string]any{"k": math.NaN()}, "float"},
		{"+Inf", map[string]any{"k": math.Inf(1)}, "float"},
		{"-Inf", map[string]any{"k": math.Inf(-1)}, "float"},
		{"invalid UTF-8 string", map[string]any{"k": "a\xffb"}, "UTF-8"},
		{"invalid UTF-8 key", map[string]any{"a\xffb": 1}, "UTF-8"},
		{"NUL string", map[string]any{"k": "a\x00b"}, "NUL"},
		{"NUL key", map[string]any{"a\x00": 1}, "NUL"},
		{"invalid UTF-8 inside an array", map[string]any{"k": []any{"ok", "\xc0\xaf"}}, "UTF-8"},
		{"depth 65", map[string]any{"k": deep(64)}, "depth"}, // object (1) + 64 arrays = 65 levels
		{"chan", map[string]any{"k": make(chan int)}, "chan int"},
		{"struct", map[string]any{"k": custom{1}}, "records_test.custom"},
		{"pointer", map[string]any{"k": new(int)}, "*int"},
		{"int32", map[string]any{"k": int32(1)}, "int32"},
		{"uint", map[string]any{"k": uint(1)}, "uint"},
		{"float32", map[string]any{"k": float32(1)}, "float32"},
		{"time.Time", map[string]any{"k": time.Unix(0, 0)}, "time.Time"},
		{"[]string", map[string]any{"k": []string{"a"}}, "[]string"},
		{"map[string]string", map[string]any{"k": map[string]string{"a": "b"}}, "map[string]string"},
		{"[]byte", map[string]any{"k": []byte("abc")}, "[]uint8"},
		{"json.Number not a number", map[string]any{"k": json.Number("abc")}, "number"},
		{"json.Number empty", map[string]any{"k": json.Number("")}, "number"},
		{"json.Number with injection", map[string]any{"k": json.Number("1,\"x\":2")}, "number"},
		{"json.Number true", map[string]any{"k": json.Number("true")}, "number"},
		{"json.Number leading zero", map[string]any{"k": json.Number("01")}, "number"},
		{"json.Number hex", map[string]any{"k": json.Number("0x10")}, "number"},
		{"json.Number NaN", map[string]any{"k": json.Number("NaN")}, "number"},
		{"json.Number plus", map[string]any{"k": json.Number("+1")}, "number"},
		{"json.Number dot", map[string]any{"k": json.Number("1.")}, "number"},
		{"top level array", []any{1}, "object"},
		{"top level string", "x", "object"},
		{"top level nil", nil, "object"},
		{"top level number", 1, "object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var (
				got string
				err error
			)
			if m, ok := c.p.(map[string]any); ok {
				got, err = records.CanonicalPayload(m)
			} else {
				got, err = records.CanonicalValue(c.p)
			}
			if err == nil {
				t.Fatalf("accepted: %s", got)
			}
			if !errors.Is(err, records.ErrInvalidPayload) || !errors.Is(err, records.ErrInvalidRecord) {
				t.Errorf("err = %v, want ErrInvalidPayload", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}

	t.Run("depth 64 is accepted", func(t *testing.T) {
		if _, err := records.CanonicalPayload(map[string]any{"k": deep(63)}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a self-referencing map is refused, quickly", func(t *testing.T) {
		m := map[string]any{}
		m["a"] = m
		m["b"] = m
		_, err := records.CanonicalPayload(m)
		if !errors.Is(err, records.ErrInvalidPayload) {
			t.Fatalf("err = %v, want ErrInvalidPayload (depth)", err)
		}
	})

	t.Run("a wide and deep tree is refused without exponential work", func(t *testing.T) {
		// 2^64 paths of depth 64 if the tree were expanded naively: the size cap
		// or the depth cap must stop it.
		leaf := map[string]any{"v": "x"}
		cur := leaf
		for i := 0; i < 70; i++ {
			next := map[string]any{"a": cur, "b": cur}
			cur = next
		}
		done := make(chan error, 1)
		go func() { _, err := records.CanonicalPayload(cur); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, records.ErrInvalidPayload) && !errors.Is(err, records.ErrRecordTooLarge) {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("canonicalPayload did not stop on a hostile tree")
		}
	})
}

func TestTimeConversion(t *testing.T) {
	note := map[string]any{"ts_note": "raw value kept in the payload"}
	conv := func(t *testing.T, tm time.Time, payload map[string]any) (int64, error) {
		t.Helper()
		p, err := records.Prepare(records.Record{
			Type: "event", ArtifactID: "art-1", Time: &records.Time{T: tm}, Payload: payload,
		}, testArt)
		if err != nil {
			return 0, err
		}
		return *p.Row().TS, nil
	}
	cases := []struct {
		name    string
		t       time.Time
		payload map[string]any
		want    int64
		err     bool
	}{
		{"1970-01-01", time.Unix(0, 0), nil, 0, false},
		{"microsecond exact", time.Unix(1700000000, 123456000), nil, 1700000000123456, false},
		{"sub-microsecond truncated", time.Unix(1700000000, 123456999), nil, 1700000000123456, false},
		{"one nanosecond", time.Unix(1700000000, 1), nil, 1700000000000000, false},
		{"last microsecond of 2099", time.Date(2099, 12, 31, 23, 59, 59, 999999999, time.UTC), nil, 4102444799999999, false},
		{"2100-01-01 needs a note", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), nil, 0, true},
		{"2100-01-01 with a note", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), note, 4102444800000000, false},
		{"one second before 1970 needs a note", time.Unix(-1, 0), nil, 0, true},
		{"one nanosecond before 1970 needs a note", time.Unix(0, -1), nil, 0, true},
		{"one second before 1970 with a note", time.Unix(-1, 0), note, -1000000, false},
		{"pre-1970 sub-microsecond truncates down", time.Unix(-2, 500000999), note, -1500000, false},
		{"year 1 with a note", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), note, -62135596800000000, false},
		{"year 9999 with a note", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), note, 253402300799000000, false},
		{"largest microsecond that fits", time.Unix(9223372036854, 775807000), note, math.MaxInt64, false},
		{"one microsecond more overflows", time.Unix(9223372036854, 775808000), note, 0, true},
		{"smallest microsecond that fits", time.Unix(-9223372036855, 224192000), note, math.MinInt64, false},
		{"one microsecond less overflows", time.Unix(-9223372036855, 224191000), note, 0, true},
		{"year 300000 overflows even with a note", time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC), note, 0, true},
		{"year -300000 overflows even with a note", time.Date(-300000, 1, 1, 0, 0, 0, 0, time.UTC), note, 0, true},
		{"zone does not matter", time.Unix(1700000000, 0).In(time.FixedZone("x", 3*3600)), nil, 1700000000000000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := conv(t, c.t, c.payload)
			if c.err {
				if !errors.Is(err, records.ErrInvalidTime) {
					t.Fatalf("got %d, %v; want ErrInvalidTime", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %d, %v; want %d", got, err, c.want)
			}
		})
	}
}

func TestParserIdentityValidation(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	ok := []records.Parser{
		{Name: "sms", Version: "1.0.0"},
		{Name: "a", Version: "1"},
		{Name: "My_Parser-2.x", Version: "v1.2.3-rc.1_b"},
		{Name: long(64), Version: long(64)},
		{Name: "sms", Version: "1", Hash: "abc123"},
		{Name: "sms", Version: "1", Hash: "sha256:" + strings.Repeat("0", 64)},
		{Name: "sms", Version: "1", Hash: long(128)},
	}
	for _, p := range ok {
		if err := records.ValidateParser(p); err != nil {
			t.Errorf("%+v rejected: %v", p, err)
		}
	}
	bad := []records.Parser{
		{Name: "", Version: "1"},
		{Name: "sms", Version: ""},
		{Name: "_sms", Version: "1"},
		{Name: "sms", Version: ".1"},
		{Name: long(65), Version: "1"},
		{Name: "sms", Version: long(65)},
		{Name: "sm s", Version: "1"},
		{Name: "sms\x00", Version: "1"},
		{Name: "smé", Version: "1"},
		{Name: "sms", Version: "1\n"},
		{Name: "sms", Version: "1", Hash: "-bad"},
		{Name: "sms", Version: "1", Hash: long(129)},
		{Name: "sms", Version: "1", Hash: "a b"},
		{Name: "sms", Version: "1", Hash: "a\x00"},
		{Name: "sms", Version: "1", Hash: "é"},
	}
	for _, p := range bad {
		err := records.ValidateParser(p)
		if !errors.Is(err, records.ErrInvalidField) || !errors.Is(err, records.ErrInvalidRecord) {
			t.Errorf("%+v: err = %v, want ErrInvalidField", p, err)
		}
	}
}
