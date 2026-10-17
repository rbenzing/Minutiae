package sqlitefile

// An undecided column default is neither a value nor a NULL: the history
// comparison never calls it "same" or "different" (Task 1b, ruling U3).

import "testing"

func vUndecided(r DefaultReason) Value { return Value{Omitted: true, DefaultUndecided: r} }

func TestUndecidedDefaultComparesUnknownNotSameOrDiffer(t *testing.T) {
	exprD, inexD := vUndecided(DefaultReasonExpression), vUndecided(DefaultReasonInexactLiteral)
	others := map[string]Value{
		"a stored NULL":                       {},
		"a stored integer":                    vInt(7),
		"a stored real":                       vFloat(1.5),
		"a stored text":                       vText("x"),
		"a stored blob":                       vBlob(1),
		"a not-stored NULL (a plain Omitted)": {Omitted: true},
		"another undecided default":           inexD,
	}
	for _, u := range []Value{exprD, inexD} {
		for name, o := range others {
			if same, known := sameValue(u, o); known || same {
				t.Errorf("undecided %v against %s: (%v, %v); want unknown", u.DefaultUndecided, name, same, known)
			}
			if same, known := sameValue(o, u); known || same {
				t.Errorf("%s against undecided %v: (%v, %v); want unknown", name, u.DefaultUndecided, same, known)
			}
		}
	}
	// compareValues: the relation is unknown with compare-incomplete; a visible
	// difference elsewhere still decides.
	a := []Value{vInt(1), inexD}
	if r := compareValues(a, []Value{vInt(1), {}}); r.kind != cmpUnknown || r.note != NoteCompareIncomplete {
		t.Errorf("same first value, undecided against NULL: %+v; want unknown/compare-incomplete", r)
	}
	if r := compareValues(a, []Value{vInt(1), inexD}); r.kind != cmpUnknown || r.note != NoteCompareIncomplete {
		t.Errorf("two undecided defaults: %+v; want unknown/compare-incomplete", r)
	}
	if r := compareValues(a, []Value{vInt(2), {}}); r.kind != cmpDiffer {
		t.Errorf("a decided difference elsewhere: %+v; want differ", r)
	}
}
