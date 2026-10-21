package utf8fold

import (
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// xTextCherokeeFold is the one place golang.org/x/text disagrees with the Unicode 12.1.0 data: its
// cases.Fold maps the Cherokee capitals U+13A0-U+13F5 to the small letters (U+AB70-U+ABBF, U+13F8-U+13FD),
// while CaseFolding.txt maps the small letters to the capitals and leaves the capitals alone. This package
// follows CaseFolding.txt, and the kernel does too: the committed vectors (category casefold) resolve
// U+AB70 against U+13A0. The disagreement is a finding for the coordinator, pinned here exactly (the 86
// code points and the small letter x/text returns) so any other disagreement, or any change in this one,
// fails the test.
func xTextCherokeeFold(r rune) (rune, bool) {
	switch {
	case r >= 0x13A0 && r <= 0x13EF:
		return 0xAB70 + (r - 0x13A0), true
	case r >= 0x13F0 && r <= 0x13F5:
		return 0x13F8 + (r - 0x13F0), true
	}
	return 0, false
}

// TestCrossCheckAgainstXText compares Key of every assigned code point that is not default-ignorable with
// norm.NFD(cases.Fold(norm.NFD(cp))) from golang.org/x/text. Its tables are of a newer Unicode version,
// which is valid for code points assigned in 12.1 by Unicode's stability policy.
func TestCrossCheckAgainstXText(t *testing.T) {
	u, err := loadUCD(ucdDir)
	if err != nil {
		t.Fatal(err)
	}
	ign := func(r rune) bool {
		for _, g := range u.ignorable {
			if r >= g.lo && r <= g.hi {
				return true
			}
		}
		return false
	}
	folder := cases.Fold()
	checked, cherokee, bad := 0, 0, 0
	for _, rg := range u.assigned {
		for r := rg.lo; r <= rg.hi; r++ {
			if ign(r) {
				continue
			}
			checked++
			want := norm.NFD.String(folder.String(norm.NFD.String(string(r))))
			got, ok := Key(nil, []byte(string(r)))
			if ok && string(got) == want {
				continue
			}
			if small, isCh := xTextCherokeeFold(r); isCh && ok && string(got) == string(r) && want == string(small) {
				cherokee++
				continue
			}
			bad++
			if bad <= 20 {
				t.Errorf("U+%04X: Key = (%q, %v), x/text = %q", r, got, ok, want)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d code points disagree", bad, checked)
	}
	if cherokee != 86 {
		t.Errorf("%d Cherokee disagreements, the pinned finding is exactly 86 (U+13A0-U+13F5)", cherokee)
	}
	t.Logf("%d code points agree; %d Cherokee capitals differ as pinned", checked-bad-cherokee, cherokee)
}
