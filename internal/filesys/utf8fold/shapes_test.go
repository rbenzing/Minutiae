package utf8fold

import (
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
	text := []string{"abc", "ABC", "café", "café", "a\x80b"}
	opaque := []string{"x\xff", "\x80x", "x\xc3", "é\x80\xff"}
	undecided := []string{"x\xffy", "\x80x\x81", "­\x80"}
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
	check("café", "café", Same)
	check("abc", "abd", Different)
	// an opaque name against ITSELF plus a lowercase z is measured; an upper-case Z is not
	check("x\xff", "x\xffz", Different)
	check("x\xff", "x\xffZ", Unknown)
	// identical bytes are the same name under any rule, even for a shape no vector shows
	check("x\xffy", "x\xffy", Same)
}
