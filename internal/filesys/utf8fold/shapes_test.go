package utf8fold

import (
	"math/rand"
	"slices"
	"testing"
)

// TestEveryDecidedShapeIsShownByAVector ties "measured, no more" to the data (ruling Z40): the shape
// classes Classify can decide are exactly the ones some committed vector name is made of. A class no
// vector shows, or a class a vector shows that decidedShapes does not list, fails.
func TestEveryDecidedShapeIsShownByAVector(t *testing.T) {
	v := readVectors(t)
	seen := map[string]int{}
	for _, r := range v.Vectors {
		for _, h := range []string{r.StoredHex, r.QueryHex} {
			name := decodeName(t, r.ID, "name", h)
			form, labels := shape(name)
			if form == FormUndecided {
				continue
			}
			for _, l := range labels {
				seen[l]++
			}
		}
	}
	for _, l := range decidedShapes {
		if seen[l] == 0 {
			t.Errorf("shape class %q is decided but no vector row shows it", l)
		}
	}
	for l, n := range seen {
		if !slices.Contains(decidedShapes, l) {
			t.Errorf("a vector shows shape class %q (%d names) that decidedShapes does not list", l, n)
		}
	}
	// every undecided vector name is a measured opaque name plus 'z', decided by Equal's one extension
	for _, r := range v.Vectors {
		for _, h := range []string{r.StoredHex, r.QueryHex} {
			name := decodeName(t, r.ID, "name", h)
			if Classify(name) == FormUndecided {
				if Equal(decodeName(t, r.ID, "query", r.QueryHex), decodeName(t, r.ID, "stored", r.StoredHex)) == Unknown {
					t.Errorf("row %d: an undecided name left the comparison Unknown", r.ID)
				}
			}
		}
	}
}

func TestEqualEmptyNameIsUnknown(t *testing.T) {
	for _, p := range [][2]string{{"", ""}, {"", "a"}, {"a", ""}, {"", "\x80"}} {
		if v := Equal([]byte(p[0]), []byte(p[1])); v != Unknown {
			t.Errorf("Equal(%q, %q) = %v, want Unknown", p[0], p[1], v)
		}
	}
	var zero Verdict
	if zero != Unknown {
		t.Error("the zero Verdict is not Unknown")
	}
}

// Every pairing of the three kinds of name is decided the same way in both argument orders (ruling Z41):
// an opaque name is Different from every other name whichever side it is on, an undecided name makes the
// answer Unknown on either side, and two text names compare by key.
func TestEqualBothOrdersForEveryKindPairing(t *testing.T) {
	text := []string{"abc", "ABC", "caf\u00e9", "cafe\u0301", "a\x80b", string(rune(0x200b))} // the last has an empty key
	opaque := []string{"x\xff", "\x80x", "x\xc3", "e\u0301\x80\xff"}
	undecided := []string{"x\xffy", "\x80x\x81", "\u00ad\x80"}
	check := func(a, b string, want Verdict) {
		t.Helper()
		if got := Equal([]byte(a), []byte(b)); got != want {
			t.Errorf("Equal(%q, %q) = %v, want %v", a, b, got, want)
		}
		if got := Equal([]byte(b), []byte(a)); got != want {
			t.Errorf("Equal(%q, %q) = %v, want %v (reversed)", b, a, got, want)
		}
	}
	for _, o := range opaque {
		for _, x := range text {
			check(o, x, Different)
		}
		for _, x := range opaque {
			if x != o {
				check(o, x, Different)
			}
		}
		for _, x := range undecided {
			check(o, x, Unknown)
		}
	}
	for _, u := range undecided {
		for _, x := range text {
			check(u, x, Unknown)
		}
	}
	check("abc", "ABC", Same)
	check("caf\u00e9", "cafe\u0301", Same)
	check("abc", "abd", Different)
	// an opaque name against ITSELF plus a lowercase z is measured; an upper-case Z is not
	check("x\xff", "x\xffz", Different)
	check("x\xff", "x\xffZ", Unknown)
	// identical bytes are the same name under any rule, even for a shape no vector shows
	check("x\xffy", "x\xffy", Same)
}

// A Query answers exactly like Equal, in a loop, without allocating per comparison.
func TestQueryMatchesEqualAndDoesNotAllocate(t *testing.T) {
	r := rand.New(rand.NewSource(5)) //nolint:gosec // a fixed-seed test generator
	names := make([][]byte, 3000)
	for i := range names {
		names[i] = randomName(r)
	}
	names = append(names, []byte("x\xff"), []byte("x\xffz"), []byte("x\xffy"), []byte("\x80x"), []byte(""))
	for _, qn := range names[:200] {
		q := NewQuery(qn)
		for _, n := range names {
			if got, want := q.Compare(n), Equal(qn, n); got != want {
				t.Fatalf("Query(%q).Compare(%q) = %v, Equal = %v", qn, n, got, want)
			}
		}
	}
	q := NewQuery([]byte("Stra" + string(rune(0xDF)) + "e"))
	other := []byte("strasse" + string(rune(0x200B)))
	q.Compare(other) // warm the scratch buffers
	if a := testing.AllocsPerRun(50, func() { q.Compare(other) }); a != 0 {
		t.Errorf("Compare allocated %v times per call, want 0", a)
	}
}
