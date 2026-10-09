package plist

import (
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// P29: a negative CF$UID is malformed however it is spelled.
func TestDecodeXMLNegativeUIDSpellings(t *testing.T) {
	spellings := map[string]string{
		"entity minus":      `<key>CF$UID</key><integer>&#45;1</integer>`,
		"entity in key":     `<key>CF&#36;UID</key><integer>-1</integer>`,
		"comment between":   `<key>CF$UID</key><!--x--><integer>-1</integer>`,
		"cdata key":         `<key><![CDATA[CF$UID]]></key><integer>-1</integer>`,
		"space in end tag":  `<key>CF$UID</key ><integer>-1</integer>`,
		"space in start":    `<key>CF$UID</key><integer >-1</integer>`,
		"attribute":         `<key>CF$UID</key><integer x="1">-1</integer>`,
		"plain":             `<key>CF$UID</key><integer>-1</integer>`,
		"whitespace":        "<key>CF$UID</key>\n  <integer>\n -7</integer>",
		"hex entity":        `<key>CF$UID</key><integer>&#x2d;3</integer>`,
		"cdata minus":       `<key>CF$UID</key><integer><![CDATA[-3]]></integer>`,
		"minus zero":        `<key>CF$UID</key><integer>-0</integer>`,
		"self-closing wrap": `<key>CF$UID</key><integer/><integer>-1</integer>`,
	}
	for name, body := range spellings {
		doc := `<plist version="1.0"><dict>` + body + `</dict></plist>`
		_, err := Decode([]byte(doc), newView(1<<20))
		if name == "self-closing wrap" {
			continue // not a CF$UID value; only checked for no panic
		}
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	// no false positives: comment and CDATA text, other keys, positive values
	for _, doc := range []string{
		`<plist><array><string><![CDATA[CF$UID</key><integer>-1]]></string></array></plist>`,
		`<plist><!-- CF$UID</key><integer>-1 --><array/></plist>`,
		`<plist><dict><key>other</key><integer>-1</integer></dict></plist>`,
		`<plist><dict><key>CF$UID</key><integer>5</integer></dict></plist>`,
		`<plist><dict><key>CF$UID</key><string>-1</string></dict></plist>`,
	} {
		if _, err := Decode([]byte(doc), newView(1<<20)); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
	}
}

// P30: the same archive gives the same outcome on every run.
func TestUnarchiveIsDeterministic(t *testing.T) {
	// M = array holding X; chain c1..c64 ends at M, so M is at depth 64 and X at 65 on that
	// path; the second top entry reaches M at depth 0.
	const n = 64
	objs := []any{}
	idxM := uint64(n + 1)
	idxX := uint64(n + 2)
	idxClass := uint64(n + 3)
	for i := 1; i <= n; i++ {
		next := uint64(i + 1)
		if i == n {
			next = idxM
		}
		objs = append(objs, map[string]any{"$class": UID(idxClass), "NS.objects": []any{UID(next)}})
	}
	objs = append(objs, map[string]any{"$class": UID(idxClass), "NS.objects": []any{UID(idxX)}}, "x", classRec("NSArray"))
	a := archive(1, objs...)
	a["$top"] = map[string]any{"a": UID(1), "b": UID(idxM)}
	var first string
	for i := range 64 {
		_, err := Unarchive(a, newView(1<<30))
		got := "ok"
		if err != nil {
			got = err.Error()
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d: %q, run 0: %q", i, got, first)
		}
	}
	// and the unknown-class fields: two bad fields, one error text
	b := archive(1, map[string]any{"$class": UID(2), "f1": UID(90), "f2": UID(91)}, classRec("T"))
	var text string
	for i := range 64 {
		_, err := Unarchive(b, newView(1<<30))
		if err == nil {
			t.Fatal("accepted")
		}
		if i == 0 {
			text = err.Error()
		} else if err.Error() != text {
			t.Fatalf("%q vs %q", err.Error(), text)
		}
	}
}

// P31: the surrogate stand-ins never collide with what the document holds.
func TestXMLStandInsNeverCollide(t *testing.T) {
	const surr = `<string>x&#xD800;y</string>`
	wantSurr := RawString{Bytes: beBytes('x', 0xD800, 'y'), UTF16: true}
	blockStart := func(k int) rune { return rune(0x10F000 - k*0x800) }
	for k := range 8 {
		for _, off := range []rune{0, 0x7FF} {
			r := blockStart(k) + off
			forms := map[string]string{
				"raw":     string(r),
				"hex":     "&#x" + strconv.FormatInt(int64(r), 16) + ";",
				"decimal": "&#" + strconv.FormatInt(int64(r), 10) + ";",
			}
			for form, text := range forms {
				doc := `<plist version="1.0"><array>` + surr + `<string>` + text + `</string></array></plist>`
				got := mustDecode(t, []byte(doc))
				if !reflect.DeepEqual(got, []any{wantSurr, string(r)}) {
					t.Fatalf("block %d off %#x %s: %#v", k, off, form, got)
				}
			}
		}
	}
	// every block occupied: refused, never a collision
	var parts []string
	for k := range 8 {
		if k < 4 {
			parts = append(parts, `<string>`+string(blockStart(k)+5)+`</string>`)
		} else {
			parts = append(parts, `<string>&#`+strconv.Itoa(int(blockStart(k))+5)+`;</string>`)
		}
	}
	all := `<plist version="1.0"><array>` + surr + strings.Join(parts, "") + `</array></plist>`
	if _, err := Decode([]byte(all), newView(1<<20)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("all blocks used: %v", err)
	}
	// without a surrogate reference the same document decodes
	plain := `<plist version="1.0"><array>` + strings.Join(parts, "") + `</array></plist>`
	if got := mustDecode(t, []byte(plain)).([]any); len(got) != 8 || got[0] != any(string(blockStart(0)+5)) {
		t.Fatalf("plain: %#v", got)
	}
	// references inside CDATA and comments are not references: they neither count as
	// collisions nor get rewritten
	var refs strings.Builder
	for k := range 8 {
		refs.WriteString("&#" + strconv.Itoa(int(blockStart(k))) + ";")
	}
	hidden := `<plist version="1.0"><array>` + surr + `<!-- ` + refs.String() + ` --><string><![CDATA[` + refs.String() + `&#xD800;]]></string></array></plist>`
	got := mustDecode(t, []byte(hidden))
	if !reflect.DeepEqual(got, []any{wantSurr, refs.String() + "&#xD800;"}) {
		t.Fatalf("hidden: %#v", got)
	}
	// a pair written as two references stays two exact units
	pair := mustDecode(t, []byte(`<plist version="1.0"><string>&#xD83D;&#xDE00;</string></plist>`))
	if !reflect.DeepEqual(pair, any(RawString{Bytes: beBytes(0xD83D, 0xDE00), UTF16: true})) {
		t.Fatalf("pair: %#v", pair)
	}
	// a stand-in in a key is refused, and so is a real surrogate there
	for _, key := range []string{"&#xD800;", string(rune(0x10F000)), "a&#55296;"} {
		doc := `<plist version="1.0"><dict><key>` + key + `</key><string>v</string><key>&#xDC00;</key><string>w</string></dict></plist>`
		if _, err := Decode([]byte(doc), newView(1<<20)); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
}

// P32: the exact edges of the date range, in both formats.
func TestDateEdges(t *testing.T) {
	xml := func(s string) any {
		return mustDecode(t, []byte(`<plist version="1.0"><date>`+s+`</date></plist>`))
	}
	wantTime := func(s string, want time.Time) {
		t.Helper()
		if tm, ok := xml(s).(time.Time); !ok || !tm.Equal(want) {
			t.Fatalf("%s: %#v, want %v", s, xml(s), want)
		}
	}
	wantRaw := func(s string, unix int64) {
		t.Helper()
		if rd, ok := xml(s).(RawDate); !ok || rd.Seconds != float64(unix-cocoaToUnix) {
			t.Fatalf("%s: %#v, want RawDate %d", s, xml(s), unix-cocoaToUnix)
		}
	}
	wantTime("9999-12-31T23:59:59Z", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	wantTime("9999-12-31T23:59:59+05:00", time.Date(9999, 12, 31, 18, 59, 59, 0, time.UTC))
	wantTime("0001-01-01T00:00:00Z", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC))
	wantTime("0001-01-01T00:00:00-01:00", time.Date(1, 1, 1, 1, 0, 0, 0, time.UTC))
	wantRaw("9999-12-31T23:59:59-05:00", 253402300800+4*3600+3599) // 10000-01-01T04:59:59Z
	wantRaw("0001-01-01T00:00:00+01:00", -62135596800-3600)        // 0000-12-31T23:00:00Z
	wantRaw("0000-01-01T00:00:00Z", -62167219200)

	bin := func(secs float64) any { return mustDecode(t, objectsPlist(dateObj(secs))) }
	endAfter := float64(dateEndAfter)
	justBelowEnd := math.Nextafter(endAfter, 0)
	justAboveFirst := math.Nextafter(float64(dateFirst), 0)
	justBelowFirst := math.Nextafter(float64(dateFirst), math.Inf(-1))
	for _, c := range []struct {
		secs float64
		raw  bool
	}{
		{float64(dateFirst), false},
		{justAboveFirst, false},
		{justBelowFirst, true},
		{justBelowEnd, false},
		{endAfter, true},
		{math.Nextafter(endAfter, math.Inf(1)), true},
	} {
		got := bin(c.secs)
		switch v := got.(type) {
		case RawDate:
			if !c.raw || v.Seconds != c.secs {
				t.Fatalf("%v: %#v", c.secs, got)
			}
		case time.Time:
			if c.raw || v.Year() < 1 || v.Year() > 9999 {
				t.Fatalf("%v: %#v", c.secs, got)
			}
		default:
			t.Fatalf("%v: %T", c.secs, got)
		}
	}
}

// M1: a list of references is charged for its slots.
func TestUnarchiveChargesListSlots(t *testing.T) {
	refs := make([]any, 1000)
	for i := range refs {
		refs[i] = UID(0)
	}
	view := newView(1 << 30)
	a := archive(1, map[string]any{"$class": UID(2), "NS.objects": refs}, classRec("NSArray"))
	if _, err := Unarchive(a, view); err != nil {
		t.Fatal(err)
	}
	if view.Used() < 1000*16 {
		t.Fatalf("Used = %d, want at least %d", view.Used(), 1000*16)
	}
}

// P34: the library reads elements by local name, so a namespace must not hide a negative UID.
func TestDecodeXMLNegativeUIDNamespacedSpellings(t *testing.T) {
	for name, body := range map[string]string{
		"prefixed key":     `<dict xmlns:x="u"><x:key>CF$UID</x:key><integer>-1</integer></dict>`,
		"prefixed integer": `<dict xmlns:x="u"><key>CF$UID</key><x:integer>-1</x:integer></dict>`,
		"both prefixed":    `<dict xmlns:x="u"><x:key>CF$UID</x:key><x:integer>-1</x:integer></dict>`,
		"default xmlns":    `<dict xmlns="u"><key>CF$UID</key><integer>-1</integer></dict>`,
		"prefixed dict":    `<x:dict xmlns:x="u"><key>CF$UID</key><integer>-1</integer></x:dict>`,
		"all prefixed":     `<x:dict xmlns:x="u"><x:key>CF$UID</x:key><x:integer>-1</x:integer></x:dict>`,
	} {
		doc := `<plist version="1.0">` + body + `</plist>`
		if _, err := Decode([]byte(doc), newView(1<<20)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
		pos := strings.Replace(doc, "-1", "1", 1)
		if got := mustDecode(t, []byte(pos)); got != any(UID(1)) {
			t.Fatalf("%s positive: %#v", name, got)
		}
	}
}
