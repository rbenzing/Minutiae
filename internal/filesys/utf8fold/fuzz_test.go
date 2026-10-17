package utf8fold

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

// FuzzKey feeds arbitrary bytes: no panic, a decided key is bounded and idempotent, Equal is symmetric, and a
// name equals itself.
func FuzzKey(f *testing.F) {
	for _, s := range []string{
		"",
		"a",
		"Stra\u00dfe",
		"STRASSE",
		"e\u0301",
		"\u00e9",
		"a\x80b",
		"\x80x",
		"x\xff",
		"x\xff\x80",
		"\u00ad\x80",
		"\uac00",
		"\u1100\u1161\u11a8",
		"a\u0323\u0301\u0323\u0301",
		"\xed\xa0\x80",
		"\xf4\x90\x80\x80",
		"x\xe2\x82",
	} {
		f.Add([]byte(s), []byte(s))
		f.Add([]byte(s), []byte("A"))
	}
	f.Fuzz(func(t *testing.T, a, b []byte) {
		for _, n := range [][]byte{a, b} {
			k, ok := Key(nil, n)
			if ok != (Classify(n) == FormUnicode) {
				t.Fatalf("Key ok = %v but Classify(%q) = %v", ok, n, Classify(n))
			}
			if !ok {
				continue
			}
			if len(n) > MaxName || len(k) > MaxName*maxKeyRunesPerRune*utf8.UTFMax {
				t.Fatalf("key of %d bytes has %d bytes", len(n), len(k))
			}
			// a key can be longer than a name may be (a character can expand to several), and Key refuses
			// names over MaxName, so the fixed point is only checked where the key is itself a name
			if len(k) <= MaxName {
				k2, ok2 := Key(nil, k)
				if !ok2 || !bytes.Equal(k, k2) {
					t.Fatalf("Key(%q) = %q is not a fixed point: (%q, %v)", n, k, k2, ok2)
				}
			}
		}
		if eq, d := Equal(a, a); !eq || !d {
			t.Fatalf("Equal(%q, %q) = (%v, %v)", a, a, eq, d)
		}
		e1, d1 := Equal(a, b)
		e2, d2 := Equal(b, a)
		if e1 != e2 || d1 != d2 {
			t.Fatalf("Equal not symmetric for %q, %q", a, b)
		}
		if !d1 && e1 {
			t.Fatalf("Equal(%q, %q) = (true, undecided)", a, b)
		}
		if e1 && d1 && !bytes.Equal(a, b) && (Classify(a) != FormUnicode || Classify(b) != FormUnicode) {
			t.Fatalf("Equal(%q, %q) folds a name that is not text", a, b)
		}
	})
}
