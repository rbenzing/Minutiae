package plist

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	howett "howett.net/plist"
)

func wantIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error %v, want %v", err, want)
	}
	if errors.Is(err, ErrInternal) {
		t.Fatalf("reached the recover guard: %v", err)
	}
}

func TestLooksLikeXML(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "<plist/>", true},
		{"bom", "\xef\xbb\xbf<plist/>", true},
		{"whitespace", " \t\r\n<plist/>", true},
		{"bom-whitespace", "\xef\xbb\xbf \n<plist/>", true},
		{"brace", "{ a = b; }", false},
		{"empty", "", false},
		{"only-space", "  \n", false},
		{"bplist", "bplist00", false},
		{"bom-only", "\xef\xbb\xbf", false},
		{"nul-first", "\x00<", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeXML([]byte(tc.in)); got != tc.want {
				t.Fatalf("LooksLikeXML(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

const xmlDecl = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

const plistHead = xmlDecl +
	`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n"

func TestPrescanXMLAcceptsRealPlists(t *testing.T) {
	arr := []any{[]any{[]any{1, 2.5}}, "uni é 世界 <&>", true, false}
	doc := map[string]any{
		"data": []byte{1, 2, 3, 4}, "date": time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		"arr": arr, "empty": map[string]any{}, "s": "", "n": int64(-7), "f": 1.5,
	}
	b, err := howett.Marshal(doc, howett.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrescanXML(b, DefaultLimits()); err != nil {
		t.Fatalf("library output: %v\n%s", err, b)
	}
	hand := plistHead + `<plist version="1.0"><!-- c --><dict><key>a</key><string><![CDATA[x < y]]></string>` +
		`<key>b</key><true/><key>c</key><array/><key>d</key><string a='>'>t</string></dict></plist>` + "\n"
	for _, in := range []string{
		hand,
		"\xef\xbb\xbf" + hand,
		strings.TrimPrefix(hand, xmlDecl),
		xmlDecl + `<plist/>`,
		"\n<plist/>",
	} {
		if err := PrescanXML([]byte(in), DefaultLimits()); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

func TestPrescanCountsItsOwnNodesAndPayload(t *testing.T) {
	doc := `<plist><array><string>abc</string><integer/><data>QQ==</data><!--ignored comment--></array></plist>`
	nodes, payload, err := prescanCore([]byte(doc), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 5 { // plist, array, string, integer, data
		t.Fatalf("nodes = %d, want 5", nodes)
	}
	if payload != 3+4+15 { // text, base64 and the comment
		t.Fatalf("payload = %d, want 22", payload)
	}
}

func TestPrescanXMLCaps(t *testing.T) {
	l := DefaultLimits()
	start := time.Now()
	t.Run("depth", func(t *testing.T) {
		ok := "<plist>" + strings.Repeat("<array>", 63) + strings.Repeat("</array>", 63) + "</plist>"
		if err := PrescanXML([]byte(ok), l); err != nil {
			t.Fatalf("depth 64: %v", err)
		}
		bad := "<plist>" + strings.Repeat("<array>", 64) + strings.Repeat("</array>", 64) + "</plist>"
		wantIs(t, PrescanXML([]byte(bad), l), ErrLimit)
	})
	t.Run("nodes", func(t *testing.T) {
		doc := "<plist><array>" + strings.Repeat("<integer/>", 1<<20+1) + "</array></plist>"
		wantIs(t, PrescanXML([]byte(doc), l), ErrLimit)
		edge := Limits{MaxNodes: 4, MaxDepth: 8, MaxPayload: 100}
		if err := PrescanXML([]byte("<plist><array><integer/><integer/></array></plist>"), edge); err != nil {
			t.Fatalf("4 nodes: %v", err)
		}
		wantIs(t, PrescanXML([]byte("<plist><array><integer/><integer/><integer/></array></plist>"), edge), ErrLimit)
	})
	t.Run("payload", func(t *testing.T) {
		big := "<plist><string>" + string(bytes.Repeat([]byte("a"), 70<<20)) + "</string></plist>"
		wantIs(t, PrescanXML([]byte(big), l), ErrLimit)
		edge := Limits{MaxNodes: 10, MaxDepth: 8, MaxPayload: 10}
		if err := PrescanXML([]byte("<plist><string>0123456789</string></plist>"), edge); err != nil {
			t.Fatalf("payload at limit: %v", err)
		}
		wantIs(t, PrescanXML([]byte("<plist><string>01234567890</string></plist>"), edge), ErrLimit)
		wantIs(t, PrescanXML([]byte("<plist><string><![CDATA[01234567890]]></string></plist>"), edge), ErrLimit)
		// text split into many runs is charged in total
		wantIs(t, PrescanXML([]byte("<plist><array><string>123456</string><string>123456</string></array></plist>"), edge), ErrLimit)
		wantIs(t, PrescanXML([]byte("<plist><string>1</string><!--"+strings.Repeat("c", 11)+"--></plist>"), edge), ErrLimit)
	})
	t.Run("dict-tags", func(t *testing.T) {
		wantIs(t, PrescanXML([]byte("<plist>"+strings.Repeat("<dict>", 100000)), l), ErrLimit)
	})
	if time.Since(start) > 20*time.Second {
		t.Fatalf("caps took %v", time.Since(start))
	}
}

func TestPrescanXMLRefusals(t *testing.T) {
	var utf16 []byte
	utf16 = append(utf16, 0xff, 0xfe)
	for _, c := range []byte("<plist/>") {
		utf16 = append(utf16, c, 0)
	}
	var utf16NoBOM []byte
	for _, c := range []byte("<plist/>") {
		utf16NoBOM = append(utf16NoBOM, c, 0)
	}
	for _, tc := range []struct {
		name string
		in   string
		want error
	}{
		{"entity", `<!DOCTYPE plist [<!ENTITY a "x">]><plist/>`, ErrUnsupported},
		{"subset-no-entity", `<!DOCTYPE plist [ ]><plist/>`, ErrUnsupported},
		{"entity-outside-brackets", `<!DOCTYPE plist "<!ENTITY"><plist/>`, ErrUnsupported},
		{"utf16le-bom", string(utf16), ErrUnsupported},
		{"utf16be-bom", "\xfe\xff\x00<\x00p", ErrUnsupported},
		{"utf16-nobom", string(utf16NoBOM), ErrUnsupported},
		{"utf16be-nobom", "\x00<\x00p\x00l", ErrUnsupported},
		{"utf32-bom", "\xff\xfe\x00\x00<\x00\x00\x00", ErrUnsupported},
		{"utf32be-bom", "\x00\x00\xfe\xff\x00\x00\x00<", ErrUnsupported},
		{"pi-midway", `<plist><?php echo 1; ?></plist>`, ErrMalformed},
		{"pi-after-space", " <?xml version=\"1.0\"?><plist/>", ErrMalformed},
		{"second-xml-decl", `<?xml version="1.0"?><?xml version="1.0"?><plist/>`, ErrMalformed},
		{"pi-xml-stylesheet", `<?xml-stylesheet href="a"?><plist/>`, ErrMalformed},
		{"unterminated-comment", `<plist><!-- never ends</plist>`, ErrMalformed},
		{"unterminated-cdata", `<plist><string><![CDATA[ x </string></plist>`, ErrMalformed},
		{"cdata-outside-root", `<![CDATA[x]]><plist/>`, ErrMalformed},
		{"unterminated-tag", `<plist><string`, ErrMalformed},
		{"unterminated-attr", `<plist a="x></plist>`, ErrMalformed},
		{"unterminated-pi", `<?xml version="1.0"`, ErrMalformed},
		{"unterminated-doctype", `<!DOCTYPE plist `, ErrMalformed},
		{"lt-at-eof", `<plist></plist><`, ErrMalformed},
		{"lt-space", `<plist>< /plist>`, ErrMalformed},
		{"bad-bang", `<plist><!ELEMENT a></plist>`, ErrMalformed},
		{"unbalanced-close", `</plist>`, ErrMalformed},
		{"unclosed-root", `<plist><array></array>`, ErrMalformed},
		{"text-outside-root", `hello<plist/>`, ErrMalformed},
		{"text-after-root", `<plist/>tail`, ErrMalformed},
		{"two-roots", `<plist/><plist/>`, ErrMalformed},
		{"doctype-after-root", `<plist/><!DOCTYPE plist>`, ErrMalformed},
		{"two-doctypes", `<!DOCTYPE plist><!DOCTYPE plist><plist/>`, ErrMalformed},
		{"no-root", " <!-- c --> ", ErrMalformed},
		{"empty", ``, ErrMalformed},
		{"non-plist-root", `<html></html>`, ErrUnsupported},
		{"openstep-data", `<deadbeef>`, ErrUnsupported},
		{"plist-prefix-name", `<plistx/>`, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantIs(t, PrescanXML([]byte(tc.in), DefaultLimits()), tc.want)
		})
	}
}

func TestPrescanXMLTruncatedEverywhere(t *testing.T) {
	doc := []byte(plistHead + `<plist version="1.0"><!-- c --><dict><key a='>'>k</key><string><![CDATA[x]]></string>` +
		`<array><integer>1</integer><true/></array></dict></plist>`)
	if err := PrescanXML(doc, DefaultLimits()); err != nil {
		t.Fatalf("control: %v", err)
	}
	for n := range len(doc) {
		// prescanCore directly: a panic here is a bug, not something a guard may absorb.
		_, _, err := prescanCore(doc[:n], DefaultLimits())
		if err == nil {
			t.Fatalf("prefix of %d bytes accepted: %q", n, doc[:n])
		}
		if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrLimit) {
			t.Fatalf("prefix %d: unclassified error %v", n, err)
		}
	}
}

func TestPrescanTimeBounded(t *testing.T) {
	for name, in := range map[string]string{
		"dash-run":     strings.Repeat("<!---", 1<<20),
		"lt-run":       strings.Repeat("<", 5<<20),
		"cdata-run":    strings.Repeat("<![CDATA[", 1<<19),
		"pi-run":       "<?xml " + strings.Repeat("<?", 1<<20),
		"quote-run":    "<plist a=" + strings.Repeat("\"", 5<<20),
		"doctype-run":  "<!DOCTYPE " + strings.Repeat("\"<!", 1<<20),
		"open-tag-run": strings.Repeat("<plist a='", 1<<19),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := PrescanXML([]byte(in), DefaultLimits())
			if err == nil {
				t.Fatal("accepted")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("took %v", d)
			}
		})
	}
}

func TestCheckDispatchesByFormat(t *testing.T) {
	l := DefaultLimits()
	bin := mustMarshal(t, map[string]any{"a": 1})
	xml, err := howett.Marshal(map[string]any{"a": 1}, howett.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(bin, l); err != nil {
		t.Fatalf("binary: %v", err)
	}
	if err := Check(xml, l); err != nil {
		t.Fatalf("xml: %v", err)
	}
	open, err := howett.Marshal(map[string]any{"a": 1}, howett.OpenStepFormat)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"openstep":       open,
		"openstep-hand":  []byte(`{ a = b; c = (1, 2); }`),
		"gnustep":        []byte(`{ a = <*I1>; }`),
		"json":           []byte(`{"a": 1}`),
		"bplist-version": []byte("bplist15" + strings.Repeat("\x00", 40)),
		"garbage":        []byte("hello"),
	} {
		t.Run(name, func(t *testing.T) { wantIs(t, Check(in, l), ErrUnsupported) })
	}
	wantIs(t, Check(nil, l), ErrMalformed)
	wantIs(t, Check([]byte{}, l), ErrMalformed)
	wantIs(t, Check([]byte("<deadbeef>"), l), ErrUnsupported)
	// the XML path charges the XML limits
	wantIs(t, Check([]byte("<plist>"+strings.Repeat("<dict>", 100)), l), ErrLimit)
	// the zero Limits value means the defaults on this path too
	if err := Check(xml, Limits{}); err != nil {
		t.Fatalf("zero limits: %v", err)
	}
}
