package plist

import (
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	howett "howett.net/plist"
)

// xmlDict wraps dictionary content in a plist document.
func xmlDict(inner string) string {
	return `<plist version="1.0"><dict>` + inner + `</dict></plist>`
}

func TestDecodeReportCountsXMLDuplicateKeys(t *testing.T) {
	doc := []byte(xmlDict(`<key>a</key><integer>1</integer><key>a</key><integer>2</integer>`))
	v, rep, err := DecodeReport(doc, newView(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"a": int64(2)}; !reflect.DeepEqual(v, want) {
		t.Fatalf("value %#v, want %#v", v, want)
	}
	if rep != (Report{DuplicateKeys: 1}) {
		t.Fatalf("report %+v, want one duplicate", rep)
	}
	view := newView(1 << 20)
	got, err := Decode(doc, view)
	var le *LossError
	if got != nil || !errors.Is(err, ErrUnsupported) || !errors.As(err, &le) || le.Report != (Report{DuplicateKeys: 1}) {
		t.Fatalf("Decode = %#v, %v; want nil and a *LossError with one duplicate", got, err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused document keeps %d bytes charged", view.Used())
	}
}

// libraryDups returns keyElements minus the entries the library kept in every dict it built.
func libraryDups(t *testing.T, doc string, keyElements int) int {
	t.Helper()
	var v any
	if _, err := howett.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("library: %v", err)
	}
	kept := 0
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			kept += len(x)
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return keyElements - kept
}

func TestXMLDuplicateCountMatchesLibrary(t *testing.T) {
	kv := func(k, v string) string { return `<key>` + k + `</key><integer>` + v + `</integer>` }
	rows := []struct {
		name string
		xml  string
		keys int
		want int
	}{
		{"plain repeat", xmlDict(kv("a", "1") + kv("a", "2")), 2, 1},
		{"three equal keys", xmlDict(kv("a", "1") + kv("a", "2") + kv("a", "3")), 3, 2},
		{
			"five spellings of one key",
			xmlDict(kv("a", "1") + kv("&#97;", "2") + kv("&#x61;", "3") + kv("<![CDATA[a]]>", "4") + kv("a<!-- c -->", "5")),
			5, 4,
		},
		{"keys that differ in case", xmlDict(kv("a", "1") + kv("A", "2")), 2, 0},
		{"surrounding whitespace", xmlDict(kv(" a ", "1") + kv("a", "2")), 2, 0},
		{"NFC and NFD forms of one letter", xmlDict(kv("é", "1") + kv("é", "2")), 2, 0},
		{"empty keys", xmlDict(`<key></key><integer>1</integer><key/><integer>2</integer>`), 2, 1},
		{
			"namespace prefix on the key element",
			xmlDict(kv("a", "1") + `<x:key xmlns:x="urn:x">a</x:key><integer>2</integer>`),
			2, 1,
		},
		{
			"repeat in the inner dict only",
			xmlDict(`<key>o</key><dict>` + kv("a", "1") + kv("a", "2") + `</dict>`),
			3, 1,
		},
		{
			"two-key CF$UID dict repeated",
			xmlDict(`<key>x</key><dict>` + kv("CF$UID", "1") + kv("CF$UID", "2") + `</dict>`),
			3, 1,
		},
		{
			"two dicts holding the same key once",
			xmlDict(`<key>p</key><dict>` + kv("a", "1") + `</dict><key>q</key><dict>` + kv("a", "2") + `</dict>`),
			4, 0,
		},
	}
	for _, r := range rows {
		if got := libraryDups(t, r.xml, r.keys); got != r.want {
			t.Errorf("%s: the library keeps %d of %d keys, want %d duplicates", r.name, r.keys-got, r.keys, r.want)
		}
		_, rep, err := DecodeReport([]byte(r.xml), newView(1<<24))
		if err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		if rep.DuplicateKeys != uint64(r.want) {
			t.Errorf("%s: DuplicateKeys = %d, want %d", r.name, rep.DuplicateKeys, r.want)
		}
	}
}

func TestDecodeReportXMLNestedDuplicates(t *testing.T) {
	doc := xmlDict(`<key>k</key><array>` +
		`<dict><key>x</key><integer>1</integer><key>x</key><integer>2</integer><key>x</key><integer>3</integer></dict>` +
		`</array><key>k</key><integer>5</integer>`)
	// The outer "k" is repeated (1) and so is "x" twice in the dict inside the first entry; the
	// library has already replaced that entry, and the scan counts keys of the document, not of
	// the surviving tree, so XML gives 3 where the binary count would give 1.
	_, rep, err := DecodeReport([]byte(doc), newView(1<<20))
	if err != nil || rep.DuplicateKeys != 3 {
		t.Fatalf("report %+v err %v; want 3", rep, err)
	}
	doc = xmlDict(`<key>k</key><integer>5</integer><key>k</key><array>` +
		`<dict><key>x</key><integer>1</integer><key>x</key><integer>2</integer><key>x</key><integer>3</integer></dict></array>`)
	if _, rep, err = DecodeReport([]byte(doc), newView(1<<20)); err != nil || rep.DuplicateKeys != 3 {
		t.Fatalf("surviving order: report %+v err %v; want 3", rep, err)
	}
}

func TestDecodeStrictAndReportAgreeOnMalformedXML(t *testing.T) {
	doc := []byte(xmlDict(`<key>a</key><integer>1</integer><key>a</key><integer>2</integer><key>b</key><integer>x</integer>`))
	view1, view2 := newView(1<<20), newView(1<<20)
	_, err1 := Decode(doc, view1)
	_, _, err2 := DecodeReport(doc, view2)
	if !errors.Is(err1, ErrMalformed) || !errors.Is(err2, ErrMalformed) {
		t.Fatalf("Decode: %v; DecodeReport: %v; want ErrMalformed from both", err1, err2)
	}
	if errors.Is(err1, ErrUnsupported) {
		t.Fatalf("the strict form reported the loss before the malformed value: %v", err1)
	}
	if view1.Used() != 0 || view2.Used() != 0 {
		t.Fatalf("charges kept: %d and %d", view1.Used(), view2.Used())
	}
}

func distinctKeysXML(n int) []byte {
	var sb strings.Builder
	sb.WriteString(`<plist><dict>`)
	for i := range n {
		sb.WriteString(`<key>k` + strconv.Itoa(i) + `</key><integer>1</integer>`)
	}
	sb.WriteString(`</dict></plist>`)
	return []byte(sb.String())
}

func TestXMLDuplicateSetIsChargedBeforeItIsBuilt(t *testing.T) {
	b := distinctKeysXML(100000)
	nodes, payload, err := measure(b, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	base := int64(len(b))*chargePerInputByte + int64(nodes)*chargePerNode + int64(payload)*chargePerPayload
	view := newView(base)
	if _, _, err := DecodeReport(b, view); !errors.Is(err, ErrNoBudget) {
		t.Fatalf("a budget without the key term: err %v, want ErrNoBudget", err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused document keeps %d bytes charged", view.Used())
	}
	view = newView(base + int64(nodes)*chargePerXMLKey)
	_, rep, err := DecodeReport(b, view)
	if err != nil || rep != (Report{}) {
		t.Fatalf("with the key term: report %+v err %v", rep, err)
	}
}

func TestXMLDuplicateKeyBombIsBounded(t *testing.T) {
	const n = 200000
	b := []byte(xmlDict(strings.Repeat(`<key>a</key><integer>1</integer>`, n)))
	view := newView(1 << 30)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	v, rep, err := DecodeReport(b, view)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"a": int64(1)}; !reflect.DeepEqual(v, want) {
		t.Fatalf("value %#v, want %#v", v, want)
	}
	if rep.DuplicateKeys != n-1 {
		t.Fatalf("DuplicateKeys = %d, want %d", rep.DuplicateKeys, n-1)
	}
	delta, charged := after.TotalAlloc-before.TotalAlloc, uint64(view.Used())
	t.Logf("TotalAlloc delta %d, charged %d", delta, charged)
	if delta > charged {
		t.Fatalf("allocated %d bytes but charged only %d", delta, charged)
	}

	huge := []byte(xmlDict(strings.Repeat(`<key>a</key>`, 1_100_000)))
	hv := newView(1 << 30)
	if _, _, err := DecodeReport(huge, hv); !errors.Is(err, ErrLimit) {
		t.Fatalf("1.1M key elements: err %v, want ErrLimit", err)
	}
	if hv.Used() != 0 {
		t.Fatalf("the refused document keeps %d bytes charged", hv.Used())
	}
}

func TestXMLDistinctKeysAreNotDuplicates(t *testing.T) {
	_, rep, err := DecodeReport(distinctKeysXML(1000), newView(1<<24))
	if err != nil || !rep.Lossless() {
		t.Fatalf("report %+v err %v", rep, err)
	}
}
