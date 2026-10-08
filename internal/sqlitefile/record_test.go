package sqlitefile_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// tVarint is a test-side varint encoder (independent of the library's).
func tVarint(v uint64) []byte {
	if v>>56 != 0 {
		out := make([]byte, 9)
		out[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return out
	}
	var groups []byte
	for {
		groups = append([]byte{byte(v & 0x7f)}, groups...)
		v >>= 7
		if v == 0 {
			break
		}
	}
	for i := 0; i < len(groups)-1; i++ {
		groups[i] |= 0x80
	}
	return groups
}

// mkRecord builds a record from serial types and the body bytes.
func mkRecord(serials []uint64, body ...[]byte) []byte {
	var s []byte
	for _, x := range serials {
		s = append(s, tVarint(x)...)
	}
	hl := len(s) + 1
	if hl >= 128 {
		hl++
	}
	return cat(tVarint(uint64(hl)), s, cat(body...))
}

func be(v uint64, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(v >> (8 * (n - 1 - i)))
	}
	return out
}

func TestRecordDecodeSerialTypes(t *testing.T) {
	big := bytes.Repeat([]byte{0xa5}, 70000)
	f15 := math.Float64bits(1.5)
	cases := []struct {
		name   string
		serial uint64
		body   []byte
		check  func(v sqlitefile.Value) bool
	}{
		{"null", 0, nil, func(v sqlitefile.Value) bool { return v.Kind == sqlitefile.KindNull }},
		{"int8 127", 1, []byte{0x7f}, intIs(127)},
		{"int8 -128", 1, []byte{0x80}, intIs(-128)},
		{"int8 -1", 1, []byte{0xff}, intIs(-1)},
		{"int16 -32768", 2, []byte{0x80, 0x00}, intIs(-32768)},
		{"int16 32767", 2, []byte{0x7f, 0xff}, intIs(32767)},
		{"int16 -2", 2, []byte{0xff, 0xfe}, intIs(-2)},
		{"int24 -1", 3, []byte{0xff, 0xff, 0xff}, intIs(-1)},
		{"int24 min", 3, []byte{0x80, 0x00, 0x00}, intIs(-8388608)},
		{"int24 max", 3, []byte{0x7f, 0xff, 0xff}, intIs(8388607)},
		{"int24 -300", 3, []byte{0xff, 0xfe, 0xd4}, intIs(-300)},
		{"int32 min", 4, []byte{0x80, 0, 0, 0}, intIs(-2147483648)},
		{"int32 max", 4, []byte{0x7f, 0xff, 0xff, 0xff}, intIs(2147483647)},
		{"int48 -1", 5, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, intIs(-1)},
		{"int48 min", 5, []byte{0x80, 0, 0, 0, 0, 0}, intIs(-140737488355328)},
		{"int48 max", 5, []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff}, intIs(140737488355327)},
		{"int64 min", 6, []byte{0x80, 0, 0, 0, 0, 0, 0, 0}, intIs(math.MinInt64)},
		{"int64 max", 6, []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, intIs(math.MaxInt64)},
		{"int64 -1", 6, bytes.Repeat([]byte{0xff}, 8), intIs(-1)},
		{"float 1.5", 7, be(f15, 8), func(v sqlitefile.Value) bool { return v.Kind == sqlitefile.KindFloat && v.Float == 1.5 }},
		{"float -inf", 7, be(math.Float64bits(math.Inf(-1)), 8), func(v sqlitefile.Value) bool {
			return v.Kind == sqlitefile.KindFloat && math.IsInf(v.Float, -1)
		}},
		{"integer 0", 8, nil, intIs(0)},
		{"integer 1", 9, nil, intIs(1)},
		{"empty blob", 12, nil, blobIs(nil)},
		{"empty text", 13, nil, textIs("")},
		{"blob of 1 (serial 14)", 14, []byte{9}, blobIs([]byte{9})},
		{"text of 1 (serial 15)", 15, []byte("x"), textIs("x")},
		{"blob of 2 (serial 16)", 16, []byte{1, 2}, blobIs([]byte{1, 2})},
		{"text of 2 (serial 17)", 17, []byte("hi"), textIs("hi")},
		{"blob of 70000", 12 + 2*70000, big, blobIs(big)},
		{"text of 70000", 13 + 2*70000, bytes.Repeat([]byte("z"), 70000), textIs(string(bytes.Repeat([]byte("z"), 70000)))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, err := sqlitefile.DecodeRecord(mkRecord([]uint64{c.serial}, c.body), sqlitefile.EncUTF8, sqlitefile.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rec.Values) != 1 || rec.Truncated || rec.Values[0].Omitted {
				t.Fatalf("record = %+v", rec)
			}
			v := rec.Values[0]
			if v.Serial != c.serial || !c.check(v) {
				t.Errorf("value = %+v", v)
			}
			if rec.BodyLen != int64(len(c.body)) || sqlitefile.SerialSize(c.serial) != int64(len(c.body)) {
				t.Errorf("BodyLen %d, SerialSize %d, body %d bytes", rec.BodyLen, sqlitefile.SerialSize(c.serial), len(c.body))
			}
		})
	}
	// Several columns at once, in order.
	rec, err := sqlitefile.DecodeRecord(mkRecord([]uint64{1, 0, 13 + 6, 8, 12 + 4}, []byte{0xfe}, []byte("abc"), []byte{7, 8}), sqlitefile.EncUTF8, sqlitefile.Limits{})
	if err != nil || len(rec.Values) != 5 || rec.Values[0].Int != -2 || rec.Values[2].Bytes == nil ||
		string(rec.Values[2].Bytes) != "abc" || rec.Values[3].Int != 0 || !bytes.Equal(rec.Values[4].Bytes, []byte{7, 8}) {
		t.Errorf("multi-column record: %+v, %v", rec, err)
	}
	if rec.HeaderLen != 6 || rec.BodyLen != 6 {
		t.Errorf("HeaderLen %d BodyLen %d, want 6 and 6", rec.HeaderLen, rec.BodyLen)
	}
}

func intIs(n int64) func(sqlitefile.Value) bool {
	return func(v sqlitefile.Value) bool {
		return v.Kind == sqlitefile.KindInt && v.Int == n && v.Len == 0 && v.Bytes == nil
	}
}

func blobIs(b []byte) func(sqlitefile.Value) bool {
	return func(v sqlitefile.Value) bool {
		return v.Kind == sqlitefile.KindBlob && v.Len == int64(len(b)) && bytes.Equal(v.Bytes, b)
	}
}

func textIs(s string) func(sqlitefile.Value) bool {
	return func(v sqlitefile.Value) bool {
		got, ok := v.Text()
		return v.Kind == sqlitefile.KindText && v.Len == int64(len(s)) && ok && got == s && v.Enc == sqlitefile.EncUTF8
	}
}

func TestFloatNaNReadsAsNull(t *testing.T) {
	for name, bits := range map[string]uint64{
		"quiet NaN":      0x7ff8000000000000,
		"signalling NaN": 0x7ff0000000000001,
		"negative NaN":   0xfff8000000000000,
		"payload NaN":    0x7fffffffffffffff,
	} {
		rec, err := sqlitefile.DecodeRecord(mkRecord([]uint64{7}, be(bits, 8)), sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		v := rec.Values[0]
		if v.Kind != sqlitefile.KindNull || v.Float != 0 || v.Omitted || v.Serial != 7 {
			t.Errorf("%s: %+v, want a NULL that remembers serial 7", name, v)
		}
	}
	rec, _ := sqlitefile.DecodeRecord(mkRecord([]uint64{7}, be(0x8000000000000000, 8)), sqlitefile.EncUTF8, sqlitefile.Limits{})
	if v := rec.Values[0]; v.Kind != sqlitefile.KindFloat || !math.Signbit(v.Float) {
		t.Errorf("-0.0 must stay a float, got %+v", v)
	}
}

func TestRecordHeaderHostile(t *testing.T) {
	bad := []struct {
		name string
		b    []byte
		lim  sqlitefile.Limits
	}{
		{"empty payload", nil, sqlitefile.Limits{}},
		{"header length 0", []byte{0x00, 0x01, 0x01}, sqlitefile.Limits{}},
		{"header length larger than the payload", []byte{0x05, 0x01}, sqlitefile.Limits{}},
		{"header length varint runs past the payload", []byte{0x81}, sqlitefile.Limits{}},
		{"serial varint overruns the header (completed by a body byte)", []byte{0x02, 0x81, 0x01}, sqlitefile.Limits{}},
		{"more columns than MaxColumns", mkRecord([]uint64{1, 1, 1, 1}, []byte{1, 2, 3, 4}), sqlitefile.Limits{MaxColumns: 3}},
		{"body length overflows", mkRecord([]uint64{math.MaxUint64, math.MaxUint64}), sqlitefile.Limits{}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := sqlitefile.DecodeRecord(c.b, sqlitefile.EncUTF8, c.lim); !errors.Is(err, sqlitefile.ErrCorrupt) {
				t.Errorf("DecodeRecord: %v, want ErrCorrupt", err)
			}
			if _, _, _, err := sqlitefile.ParseRecordHeader(c.b, max(c.lim.MaxColumns, 2000)); !errors.Is(err, sqlitefile.ErrCorrupt) && c.lim.MaxColumns == 0 {
				t.Errorf("ParseRecordHeader: %v, want ErrCorrupt", err)
			}
		})
	}
	t.Run("more than the default MaxColumns", func(t *testing.T) {
		serials := make([]uint64, 2001)
		if _, err := sqlitefile.DecodeRecord(mkRecord(serials), sqlitefile.EncUTF8, sqlitefile.Limits{}); !errors.Is(err, sqlitefile.ErrCorrupt) {
			t.Errorf("2001 columns: %v, want ErrCorrupt", err)
		}
		if _, err := sqlitefile.DecodeRecord(mkRecord(serials[:2000]), sqlitefile.EncUTF8, sqlitefile.Limits{}); err != nil {
			t.Errorf("2000 columns: %v", err)
		}
	})
	t.Run("a header of length 1 holds no columns", func(t *testing.T) {
		// The engine reads such a record as one with no stored columns.
		rec, err := sqlitefile.DecodeRecord([]byte{0x01, 0xff}, sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil || len(rec.Values) != 0 || rec.HeaderLen != 1 || rec.Truncated {
			t.Errorf("(%+v, %v)", rec, err)
		}
	})
	t.Run("body shorter than declared", func(t *testing.T) {
		b := mkRecord([]uint64{1, 4, 13 + 8}, []byte{5}, []byte{0, 0}) // int8, int32 (2 of 4 bytes), text of 4 (absent)
		rec, err := sqlitefile.DecodeRecord(b, sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Truncated || rec.Values[0].Omitted || rec.Values[0].Int != 5 {
			t.Errorf("the value wholly inside is kept: %+v", rec)
		}
		for i := 1; i <= 2; i++ {
			if !rec.Values[i].Omitted || rec.Values[i].Bytes != nil {
				t.Errorf("value %d reaches past the payload and must be omitted, got %+v", i, rec.Values[i])
			}
		}
		if rec.Values[2].Kind != sqlitefile.KindText || rec.Values[2].Len != 4 || rec.Values[1].Kind != sqlitefile.KindInt {
			t.Errorf("an omitted value keeps its kind and length: %+v %+v", rec.Values[1], rec.Values[2])
		}
		// A strict user compares the declared body with what is there.
		_, hl, body, err := sqlitefile.ParseRecordHeader(b, 100)
		if err != nil || body != 1+4+4 || int64(len(b)-hl) >= body {
			t.Errorf("ParseRecordHeader: header %d body %d err %v for %d bytes: the strict check must see the shortfall", hl, body, err, len(b))
		}
	})
	t.Run("body longer than declared is tolerated", func(t *testing.T) {
		rec, err := sqlitefile.DecodeRecord(cat(mkRecord([]uint64{1}, []byte{9}), []byte("trailing junk")), sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil || rec.Truncated || rec.Values[0].Int != 9 {
			t.Errorf("(%+v, %v)", rec, err)
		}
	})
	t.Run("a non-canonical header length varint is accepted", func(t *testing.T) {
		// A non-canonical but valid header length varint (two bytes for 2).
		b := []byte{0x80, 0x03, 0x01, 0x2a}
		serials, hl, body, err := sqlitefile.ParseRecordHeader(b, 10)
		if err != nil || hl != 3 || len(serials) != 1 || body != 1 {
			t.Errorf("(%v, %d, %d, %v)", serials, hl, body, err)
		}
	})
}

// checkValueInvariants asserts the "Value invariants" of the format
// description for one value.
func checkValueInvariants(t *testing.T, what string, v sqlitefile.Value) {
	t.Helper()
	switch v.Kind {
	case sqlitefile.KindNull, sqlitefile.KindInt, sqlitefile.KindFloat:
		if v.Len != 0 || v.Bytes != nil || v.Clipped {
			t.Errorf("%s: a %v value must have Len 0 and no Bytes: %+v", what, v.Kind, v)
		}
	case sqlitefile.KindText, sqlitefile.KindBlob:
		switch {
		case v.Omitted:
			if v.Bytes != nil || v.Clipped {
				t.Errorf("%s: an omitted value has no Bytes: %+v", what, v)
			}
		case v.Clipped:
			if int64(len(v.Bytes)) >= v.Len {
				t.Errorf("%s: a clipped value holds fewer bytes than Len: %d of %d", what, len(v.Bytes), v.Len)
			}
		default:
			if int64(len(v.Bytes)) != v.Len {
				t.Errorf("%s: len(Bytes) = %d, Len = %d", what, len(v.Bytes), v.Len)
			}
		}
	}
	if v.Kind != sqlitefile.KindNull && v.Omitted && v.Kind != sqlitefile.KindText && v.Kind != sqlitefile.KindBlob && (v.Int != 0 || v.Float != 0) {
		t.Errorf("%s: an omitted scalar carries no number: %+v", what, v)
	}
}

func TestValueInvariants(t *testing.T) {
	text := func(n int) ([]uint64, []byte) { return []uint64{uint64(13 + 2*n)}, bytes.Repeat([]byte("t"), n) }
	t.Run("every kind, live", func(t *testing.T) {
		rec, err := sqlitefile.DecodeRecord(mkRecord([]uint64{0, 1, 7, 15, 14, 8, 9},
			[]byte{5}, be(math.Float64bits(2.5), 8), []byte("x"), []byte{1}), sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range rec.Values {
			checkValueInvariants(t, "column "+string(rune('0'+i)), v)
			if v.Omitted {
				t.Errorf("column %d unexpectedly omitted", i)
			}
		}
	})
	t.Run("an over-cap live text is omitted with its true length", func(t *testing.T) {
		s, b := text(11)
		rec, err := sqlitefile.DecodeRecord(mkRecord(s, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxTextBytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		v := rec.Values[0]
		checkValueInvariants(t, "text", v)
		if !v.Omitted || v.Len != 11 || v.Bytes != nil || v.Kind != sqlitefile.KindText {
			t.Errorf("value = %+v", v)
		}
		// At the cap exactly it is kept.
		s, b = text(10)
		rec, _ = sqlitefile.DecodeRecord(mkRecord(s, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxTextBytes: 10})
		if rec.Values[0].Omitted || len(rec.Values[0].Bytes) != 10 {
			t.Errorf("text of exactly the cap: %+v", rec.Values[0])
		}
	})
	t.Run("an over-cap live blob is omitted; the blob cap is separate", func(t *testing.T) {
		b := bytes.Repeat([]byte{1}, 20)
		rec, _ := sqlitefile.DecodeRecord(mkRecord([]uint64{12 + 40}, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxBlobBytes: 19, MaxTextBytes: 1000})
		checkValueInvariants(t, "blob", rec.Values[0])
		if v := rec.Values[0]; !v.Omitted || v.Len != 20 {
			t.Errorf("blob over MaxBlobBytes: %+v", v)
		}
		rec, _ = sqlitefile.DecodeRecord(mkRecord([]uint64{12 + 40}, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxBlobBytes: 1000, MaxTextBytes: 5})
		if v := rec.Values[0]; v.Omitted {
			t.Errorf("a blob is not limited by MaxTextBytes: %+v", v)
		}
	})
	t.Run("an over-cap recovered value is clipped", func(t *testing.T) {
		s, b := text(20)
		rec, err := sqlitefile.DecodeRecovered(mkRecord(s, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxRecoveredValueBytes: 8})
		if err != nil {
			t.Fatal(err)
		}
		v := rec.Values[0]
		checkValueInvariants(t, "recovered", v)
		if !v.Clipped || len(v.Bytes) != 8 || v.Len != 20 || v.Omitted || !bytes.Equal(v.Bytes, b[:8]) {
			t.Errorf("value = %+v", v)
		}
		// The recovered cap does not touch a live decode, and a live decode does not clip.
		rec, _ = sqlitefile.DecodeRecord(mkRecord(s, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxRecoveredValueBytes: 8})
		if v := rec.Values[0]; v.Clipped || v.Omitted || len(v.Bytes) != 20 {
			t.Errorf("live value must ignore MaxRecoveredValueBytes: %+v", v)
		}
		// At the cap exactly nothing is clipped.
		s, b = text(8)
		rec, _ = sqlitefile.DecodeRecovered(mkRecord(s, b), sqlitefile.EncUTF8, sqlitefile.Limits{MaxRecoveredValueBytes: 8})
		if v := rec.Values[0]; v.Clipped || len(v.Bytes) != 8 {
			t.Errorf("recovered value of exactly the cap: %+v", v)
		}
	})
	t.Run("MaxRowBytes omits the later values of a row", func(t *testing.T) {
		b := mkRecord([]uint64{13 + 20, 13 + 20, 13 + 6, 1}, bytes.Repeat([]byte("a"), 10), bytes.Repeat([]byte("b"), 10), []byte("ccc"), []byte{4})
		rec, err := sqlitefile.DecodeRecord(b, sqlitefile.EncUTF8, sqlitefile.Limits{MaxRowBytes: 15})
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range rec.Values {
			checkValueInvariants(t, "row value", v)
			wantOmit := i == 1 || i == 2
			if v.Omitted != wantOmit {
				t.Errorf("column %d: omitted = %v, want %v (%+v)", i, v.Omitted, wantOmit, v)
			}
		}
		if rec.Values[1].Len != 10 || rec.Values[2].Len != 3 {
			t.Errorf("omitted values keep their length: %+v %+v", rec.Values[1], rec.Values[2])
		}
		if rec.Values[3].Omitted || rec.Values[3].Int != 4 {
			t.Errorf("an integer after the cap is still read: %+v", rec.Values[3])
		}
	})
	t.Run("values past the payload are omitted", func(t *testing.T) {
		rec, _ := sqlitefile.DecodeRecord(mkRecord([]uint64{1, 6, 13 + 10}, []byte{1}), sqlitefile.EncUTF8, sqlitefile.Limits{})
		for _, v := range rec.Values {
			checkValueInvariants(t, "past end", v)
		}
		if !rec.Truncated || rec.Values[0].Omitted || !rec.Values[1].Omitted || !rec.Values[2].Omitted {
			t.Errorf("%+v", rec)
		}
	})
}

func TestTextEncodingsDecode(t *testing.T) {
	cases := []struct {
		name string
		enc  sqlitefile.Encoding
		b    []byte
		want string
		ok   bool
	}{
		{"utf8 ascii", sqlitefile.EncUTF8, []byte("abc"), "abc", true},
		{"utf8 multibyte", sqlitefile.EncUTF8, []byte("héllo ☃ 𝄞"), "héllo ☃ 𝄞", true},
		{"utf8 with NUL", sqlitefile.EncUTF8, []byte("a\x00b"), "a\x00b", true},
		{"utf8 empty", sqlitefile.EncUTF8, []byte{}, "", true},
		{"utf8 invalid byte", sqlitefile.EncUTF8, []byte{0xff, 0xfe}, "", false},
		{"utf8 truncated rune", sqlitefile.EncUTF8, []byte{'a', 0xe2, 0x82}, "", false},
		{"utf8 overlong", sqlitefile.EncUTF8, []byte{0xc0, 0x80}, "", false},
		{"utf8 encoded surrogate", sqlitefile.EncUTF8, []byte{0xed, 0xa0, 0x80}, "", false},
		{"zero encoding reads as utf8", 0, []byte("ok"), "ok", true},
		{"utf16le bmp", sqlitefile.EncUTF16LE, []byte{0x41, 0x00, 0xe9, 0x00, 0x03, 0x26}, "Aé☃", true},
		{"utf16le astral", sqlitefile.EncUTF16LE, []byte{0x34, 0xd8, 0x1e, 0xdd}, "𝄞", true},
		{"utf16le empty", sqlitefile.EncUTF16LE, []byte{}, "", true},
		{"utf16le BOM kept", sqlitefile.EncUTF16LE, []byte{0xff, 0xfe, 0x41, 0x00}, "" + string(rune(0xfeff)) + "A", true},
		{"utf16le unpaired high", sqlitefile.EncUTF16LE, []byte{0x34, 0xd8}, "", false},
		{"utf16le unpaired high before text", sqlitefile.EncUTF16LE, []byte{0x34, 0xd8, 0x41, 0x00}, "", false},
		{"utf16le unpaired low", sqlitefile.EncUTF16LE, []byte{0x1e, 0xdd}, "", false},
		{"utf16le reversed pair", sqlitefile.EncUTF16LE, []byte{0x1e, 0xdd, 0x34, 0xd8}, "", false},
		{"utf16le odd byte count", sqlitefile.EncUTF16LE, []byte{0x41, 0x00, 0x42}, "", false},
		{"utf16be bmp", sqlitefile.EncUTF16BE, []byte{0x00, 0x41, 0x00, 0xe9, 0x26, 0x03}, "Aé☃", true},
		{"utf16be astral", sqlitefile.EncUTF16BE, []byte{0xd8, 0x34, 0xdd, 0x1e}, "𝄞", true},
		{"utf16be BOM kept", sqlitefile.EncUTF16BE, []byte{0xfe, 0xff, 0x00, 0x41}, "" + string(rune(0xfeff)) + "A", true},
		{"utf16be unpaired high", sqlitefile.EncUTF16BE, []byte{0xd8, 0x34}, "", false},
		{"utf16be unpaired low", sqlitefile.EncUTF16BE, []byte{0xdd, 0x1e, 0x00, 0x41}, "", false},
		{"utf16be odd byte count", sqlitefile.EncUTF16BE, []byte{0x00}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Through a record, as the reader produces it.
			rec, err := sqlitefile.DecodeRecord(mkRecord([]uint64{uint64(13 + 2*len(c.b))}, c.b), c.enc, sqlitefile.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			v := rec.Values[0]
			got, ok := v.Text()
			if ok != c.ok || got != c.want {
				t.Errorf("Text() = (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
			if !bytes.Equal(v.Bytes, c.b) {
				t.Errorf("Bytes = % x, want the raw % x", v.Bytes, c.b)
			}
		})
	}
	// Not text, or not materialized: no text.
	if s, ok := (sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte("abc")}).Text(); ok || s != "" {
		t.Errorf("a blob is not text: (%q, %v)", s, ok)
	}
	if s, ok := (sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 5}).Text(); ok || s != "" {
		t.Errorf("an integer is not text: (%q, %v)", s, ok)
	}
	if s, ok := (sqlitefile.Value{Kind: sqlitefile.KindText, Omitted: true, Len: 3}).Text(); ok || s != "" {
		t.Errorf("an omitted text has no text: (%q, %v)", s, ok)
	}
}

// TestRecordReservedSerialReadsAsNull: serial types 10 and 11 are zero-width
// NULLs, as the engine reads them (TestEngineReservedSerialTypesReadAsNull);
// the record is not rejected and the columns after them keep their offsets.
func TestRecordReservedSerialReadsAsNull(t *testing.T) {
	for _, s := range []uint64{10, 11} {
		b := mkRecord([]uint64{1, s, 1}, []byte{7, 9})
		rec, err := sqlitefile.DecodeRecord(b, sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil {
			t.Fatalf("DecodeRecord with serial %d: %v", s, err)
		}
		if rec.Truncated || rec.Reserved != 1 {
			t.Errorf("serial %d: Truncated %v Reserved %d", s, rec.Truncated, rec.Reserved)
		}
		if v := rec.Values[1]; v.Kind != sqlitefile.KindNull || v.Omitted || v.Len != 0 || v.Serial != s {
			t.Errorf("serial %d: %+v", s, v)
		}
		if rec.Values[0].Int != 7 || rec.Values[2].Int != 9 {
			t.Errorf("serial %d: neighbours %+v %+v", s, rec.Values[0], rec.Values[2])
		}
		if _, _, body, err := sqlitefile.ParseRecordHeader(b, 100); err != nil || body != 2 {
			t.Errorf("ParseRecordHeader with serial %d: body %d, %v", s, body, err)
		}
		if sqlitefile.SerialSize(s) != 0 {
			t.Errorf("SerialSize(%d) = %d, want 0", s, sqlitefile.SerialSize(s))
		}
	}
}
