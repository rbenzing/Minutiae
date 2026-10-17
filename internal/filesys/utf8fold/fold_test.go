package utf8fold

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestSupported(t *testing.T) {
	if !Supported(EncodingUTF8v121) || Supported(0) || Supported(2) {
		t.Fatalf("Supported(1,0,2) = %v %v %v, want true false false", Supported(1), Supported(0), Supported(2))
	}
	if Version != "12.1.0" || MaxName != 255 {
		t.Fatalf("Version %q MaxName %d", Version, MaxName)
	}
}

// Each expectation below is also a row, or a class of rows, of the committed kernel vectors (case folding,
// the Turkic non-mapping, U+212A, composed and decomposed forms, mark reordering, Hangul, ignorable code
// points); the empty name and the length bounds are not vector rows and follow from the bound of the package.
func TestEqualBasics(t *testing.T) {
	long := func(n int) string { return string(bytes.Repeat([]byte("a"), n)) }
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"E", "e", true},
		{"ss", "SS", true},
		{"ss", "ß", true},
		{"ß", "ẞ", true},
		{"ı", "I", false}, // no Turkic mapping
		{"ı", "i", false},
		{"İ", "i̇", true},
		{"K", "k", true}, // Kelvin sign
		{"K", "K", true},
		{"é", "é", true},
		{"É", "é", true},
		{"ạ́", "ạ́", true},
		{"가", "가", true},
		{"각", "각", true},
		{"a­b", "ab", true}, // U+00AD is ignorable
		{"a​b", "AB", true},
		{"a", "b", false},
		{"", "", true},
		{"", "a", false},
		{long(255), long(255), true},
		{long(255), "A" + long(254), true},
		{"ς", "Σ", true},
		{"σ", "ς", true},
		{"Ꟈ", "ꟈ", false}, // assigned after 12.1: no case mapping
	} {
		for _, p := range [][2]string{{c.a, c.b}, {c.b, c.a}} {
			eq, decided := Equal([]byte(p[0]), []byte(p[1]))
			if !decided || eq != c.want {
				t.Errorf("Equal(%q, %q) = (%v, decided %v), want (%v, true)", p[0], p[1], eq, decided, c.want)
			}
		}
	}
	// a 256-byte name is not comparable as a filesystem name
	if _, ok := Key(nil, []byte(long(256))); ok {
		t.Error("Key accepted a 256-byte name")
	}
	if eq, decided := Equal([]byte(long(256)), []byte(long(256))); !eq || !decided {
		t.Errorf("identical 256-byte names: (%v, %v), want (true, true): an identical name is the same name", eq, decided)
	}
	if eq, decided := Equal([]byte(long(256)), []byte(long(255))); eq || decided {
		t.Errorf("256 versus 255 bytes: (%v, %v), want (false, false)", eq, decided)
	}
}

// The classification is the measured rule (README "Casefold lookup oracle"): text, text with stray
// continuation bytes, opaque names (compared byte for byte), and everything else undecided.
func TestClassifyMatchesTheMeasuredRule(t *testing.T) {
	for _, c := range []struct {
		name string
		want Form
	}{
		{"", FormUnicode},
		{"abc", FormUnicode},
		{"éK", FormUnicode},
		{"￾￿\U0010ffff", FormUnicode}, // noncharacters and the last code point are valid
		{"x\x80", FormUnicode},        // a stray byte after the first byte is text
		{"a\x80é\x81", FormUnicode},
		{"a\x80b\x81c", FormUnicode},
		{"\x80x", FormOpaque}, // a stray byte first: opaque
		{"\x80\x80", FormOpaque},
		{"\x80é", FormOpaque},
		{"x\xff", FormOpaque}, // a lead byte truncated at the end
		{"x\xc3", FormOpaque},
		{"x\xe2\x82", FormOpaque},
		{"x\xc1\x81", FormOpaque}, // overlong
		{"x\xc0\xaf", FormOpaque},
		{"x\xed\xa0\x80", FormOpaque}, // a surrogate
		{"x\xf4\x90\x80\x80", FormOpaque},
		{"x\xf8\x88\x80\x80\x80", FormOpaque},
		{"x\xfe\xfe", FormOpaque},
		{"x\x80\xff", FormOpaque}, // a stray byte mixed with another invalid form
		{"x\xff\x80", FormOpaque}, // a stray byte after an invalid lead
		{"é\x80\xff", FormOpaque},
		// shapes no measurement covers
		{"\xff", FormUndecided},                      // nothing before the invalid byte
		{"x\xffy", FormUndecided},                    // text after the invalid byte
		{"x\xffé", FormUndecided},                    // a valid character after the invalid byte
		{"x\xc3x", FormUndecided},                    // a truncated lead followed by text
		{"\x80\xff", FormUndecided},                  // a leading stray byte with an invalid form
		{"x\xff\x80\x80\x80\x80\x80", FormUndecided}, // a tail longer than any measured
		{"­\x80", FormUndecided},                     // an ignorable character with a stray byte
		{"a\x80­b", FormUndecided},
	} {
		if got := Classify([]byte(c.name)); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestKeyRejectsInvalidUTF8(t *testing.T) {
	for _, n := range []string{"\x80x", "x\xff", "x\xc1\x81", "x\xed\xa0\x80", "x\xf4\x90\x80\x80", "x\xfe\xfe", "x\x80\xff", "\xff", "x\xffy"} {
		if k, ok := Key([]byte("keep"), []byte(n)); ok || string(k) != "keep" {
			t.Errorf("Key(%q) = (%q, %v), want dst back unchanged and ok false", n, k, ok)
		}
	}
	for _, n := range []string{"x\x80", "a\x80é", "￾"} {
		if _, ok := Key(nil, []byte(n)); !ok {
			t.Errorf("Key(%q) not ok", n)
		}
	}
}

// Undecided and opaque names: never folded, never a guessed verdict.
func TestEqualUndecidedAndOpaque(t *testing.T) {
	for _, c := range []struct {
		a, b           string
		equal, decided bool
	}{
		{"x\xff", "X\xff", false, true}, // opaque: different bytes are different names
		{"x\xff", "x\xff", true, true},  // identical bytes are the same name under any rule
		{"x\xff", "x", false, true},     // opaque against text
		{"\x80x", "\x80X", false, true},
		{"x\xffy", "X\xffY", false, false}, // undecided
		{"x\xffy", "x\xffy", true, true},
		{"x\xffy", "x", false, false},
		{"x\xffy", "x\xff", false, false},         // undecided against opaque
		{"x\xc1\x81z", "x\xc1\x81", false, true},  // measured: the negative control of an invalid row
		{"x\xc1\x81y", "x\xc1\x81", false, false}, // another extension was not measured
		{"­\x80", "\x80", false, false},
	} {
		for _, p := range [][2]string{{c.a, c.b}, {c.b, c.a}} {
			eq, decided := Equal([]byte(p[0]), []byte(p[1]))
			if eq != c.equal || decided != c.decided {
				t.Errorf("Equal(%q, %q) = (%v, %v), want (%v, %v)", p[0], p[1], eq, decided, c.equal, c.decided)
			}
		}
	}
}

func randomName(r *rand.Rand) []byte {
	parts := []string{"a", "A", "e", "E", "k", "K", "K", "é", "É", "́", "̣", "ß", "ss", "SS", "ẞ", "ı", "I", "i",
		"­", "​", "가", "ᄀ", "ᅡ", "ᆨ", "σ", "ς", "Σ", "İ", "\x80", "\x81", "x", "ﬃ", "Ω", "ω"}
	var b []byte
	for n := r.Intn(8); n > 0; n-- {
		b = append(b, parts[r.Intn(len(parts))]...)
	}
	return b
}

func TestKeyIsIdempotent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		n := randomName(r)
		k, ok := Key(nil, n)
		if !ok {
			continue
		}
		k2, ok2 := Key(nil, k)
		if !ok2 || !bytes.Equal(k, k2) {
			t.Fatalf("Key(%q) = %q, Key of that = (%q, %v)", n, k, k2, ok2)
		}
	}
}

func TestEqualIsSymmetricAndTransitiveOverSample(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	names := make([][]byte, 20000)
	for i := range names {
		names[i] = randomName(r)
	}
	first := map[string][]byte{}
	for i, a := range names {
		b := names[(i*7919+13)%len(names)]
		e1, d1 := Equal(a, b)
		e2, d2 := Equal(b, a)
		if e1 != e2 || d1 != d2 {
			t.Fatalf("Equal(%q, %q) = (%v, %v) but reversed (%v, %v)", a, b, e1, d1, e2, d2)
		}
		if Classify(a) != FormUnicode {
			continue
		}
		k, _ := Key(nil, a)
		if rep, ok := first[string(k)]; ok {
			// transitivity: everything with one key is equal to the first of its class, hence to each other
			if eq, d := Equal(a, rep); !eq || !d {
				t.Fatalf("%q and %q share key %q but Equal = (%v, %v)", a, rep, k, eq, d)
			}
		} else {
			first[string(k)] = a
		}
	}
}
