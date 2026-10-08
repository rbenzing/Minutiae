package sqlitefile

// Pins of the value comparison and the row digest of the history-row code
// (Task 13 review I-2 and I-3): values compare by storage kind AND exact bytes,
// so a changed value is never reported as equal to its live row (and silently
// dropped as DuplicateOfLive), and the WITHOUT ROWID digest separates every
// kind, every byte and every length.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
)

func vInt(n int64) Value            { return Value{Kind: KindInt, Int: n} }
func vFloat(f float64) Value        { return Value{Kind: KindFloat, Float: f} }
func vText(s string) Value          { return Value{Kind: KindText, Bytes: []byte(s), Len: int64(len(s))} }
func vBlob(b ...byte) Value         { return Value{Kind: KindBlob, Bytes: b, Len: int64(len(b))} }
func vOmitted(v Value) Value        { v.Omitted, v.Bytes = true, nil; return v }
func vClipped(v Value, n int) Value { v.Clipped, v.Bytes = true, v.Bytes[:n]; return v }

func TestSameValueTable(t *testing.T) {
	nan1 := math.Float64frombits(0x7ff8000000000001)
	nan2 := math.Float64frombits(0x7ff8000000000002)
	tests := []struct {
		name        string
		a, b        Value
		same, known bool
	}{
		{"int equal", vInt(1), vInt(1), true, true},
		{"int differ", vInt(1), vInt(2), false, true},
		{"int vs text of the same digits", vInt(1), vText("1"), false, true},
		{"int vs float of the same number", vInt(1), vFloat(1), false, true},
		{"text vs blob of the same bytes", vText("a"), vBlob('a'), false, true},
		{"null vs int zero", Value{}, vInt(0), false, true},
		{"null vs null", Value{}, Value{}, true, true},
		{"omitted null vs null", Value{Omitted: true}, Value{}, false, true},
		{"float equal", vFloat(1.5), vFloat(1.5), true, true},
		{"float differ", vFloat(1.5), vFloat(2.5), false, true},
		{"float zero vs negative zero", vFloat(0), vFloat(math.Copysign(0, -1)), false, true},
		{"float identical NaN bits", vFloat(nan1), vFloat(nan1), true, true},
		{"float NaN payloads differ", vFloat(nan1), vFloat(nan2), false, true},
		{"text equal", vText("abc"), vText("abc"), true, true},
		{"text same length differs", vText("abc"), vText("abd"), false, true},
		{"text length differs", vText("abc"), vText("ab"), false, true},
		{"text length differs, one omitted", vOmitted(vText("abc")), vText("ab"), false, true},
		{"text length differs, one clipped", vClipped(vText("abc"), 2), vText("ab"), false, true},
		{"blob equal", vBlob(1, 2), vBlob(1, 2), true, true},
		{"blob differs", vBlob(1, 2), vBlob(1, 3), false, true},
		{"a omitted, same length", vOmitted(vText("abc")), vText("abc"), false, false},
		{"b omitted, same length", vText("abc"), vOmitted(vText("abc")), false, false},
		{"a clipped, same length", vClipped(vText("abc"), 2), vText("abc"), false, false},
		{"b clipped, same length", vText("abc"), vClipped(vText("abc"), 2), false, false},
		{"empty text equal", vText(""), vText(""), true, true},
		{"empty text vs empty blob", vText(""), vBlob(), false, true},
	}
	for _, tc := range tests {
		same, known := sameValue(tc.a, tc.b)
		if same != tc.same || known != tc.known {
			t.Errorf("%s: sameValue = (%v, %v), want (%v, %v)", tc.name, same, known, tc.same, tc.known)
		}
	}
}

func TestCompareValuesOrder(t *testing.T) {
	a := []Value{vInt(1), vText("x"), vOmitted(vText("yy"))}
	if r := compareValues(a, []Value{vInt(1), vText("x"), vText("yy")}); r.kind != cmpUnknown || r.note != NoteCompareIncomplete {
		t.Errorf("an omitted value hides the answer: %+v", r)
	}
	if r := compareValues(a, []Value{vInt(1), vText("z"), vText("yy")}); r.kind != cmpDiffer {
		t.Errorf("a known difference beats an unknown one: %+v", r)
	}
	if r := compareValues(a, []Value{vInt(2), vText("x"), vText("yy")}); r.kind != cmpDiffer {
		t.Errorf("a known difference before the unknown one: %+v", r)
	}
	if r := compareValues([]Value{vInt(1)}, []Value{vInt(1), vInt(2)}); r.kind != cmpDiffer {
		t.Errorf("different widths differ: %+v", r)
	}
	if r := compareValues([]Value{vInt(1), vText("a")}, []Value{vInt(1), vText("a")}); r.kind != cmpSame {
		t.Errorf("equal rows are the same: %+v", r)
	}
}

// handDigest builds the digest the frozen encoding gives, byte by byte:
// the kind byte, then 8 big-endian bytes for an integer or a float's bit
// pattern, or an 8-byte length and the bytes for text and blob.
func handDigest(parts ...[]byte) [32]byte {
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	return sha256.Sum256(all)
}

func be64(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

func TestDigestOfGoldenVector(t *testing.T) {
	vals := []Value{{}, vInt(7), vFloat(1.5), vText("ab"), vBlob(1, 2, 3)}
	want := handDigest(
		[]byte{byte(KindNull)},
		[]byte{byte(KindInt)}, be64(7),
		[]byte{byte(KindFloat)}, be64(math.Float64bits(1.5)),
		[]byte{byte(KindText)}, be64(2), []byte("ab"),
		[]byte{byte(KindBlob)}, be64(3), []byte{1, 2, 3},
	)
	got, complete := digestOf(vals, nil)
	if got != want || !complete {
		t.Errorf("digest %x complete %v, want %x true", got, complete, want)
	}
	// the key digest hashes the chosen positions, in the given order
	sub, _ := digestOf(vals, []int{3, 1})
	wantSub := handDigest([]byte{byte(KindText)}, be64(2), []byte("ab"), []byte{byte(KindInt)}, be64(7))
	if sub != wantSub {
		t.Errorf("key digest %x, want %x", sub, wantSub)
	}
}

func TestDigestOfEveryFieldChangesIt(t *testing.T) {
	base, _ := digestOf([]Value{vInt(1)}, nil)
	for _, tc := range []struct {
		name string
		vals []Value
	}{
		{"kind: float with the same bits", []Value{{Kind: KindFloat, Float: math.Float64frombits(1)}}},
		{"kind: text 1", []Value{vText("1")}},
		{"integer bytes", []Value{vInt(2)}},
		{"float bytes", []Value{vFloat(1)}},
		{"null", []Value{{}}},
		{"two values", []Value{vInt(1), vInt(1)}},
	} {
		if d, _ := digestOf(tc.vals, nil); d == base {
			t.Errorf("%s: the digest did not change", tc.name)
		}
	}
	a, _ := digestOf([]Value{vText("ab"), vText("c")}, nil)
	b, _ := digestOf([]Value{vText("a"), vText("bc")}, nil)
	if a == b {
		t.Error("the length prefix does not separate (ab, c) from (a, bc)")
	}
	x, _ := digestOf([]Value{vText("ab")}, nil)
	y, _ := digestOf([]Value{vText("ac")}, nil)
	if x == y {
		t.Error("text bytes do not change the digest")
	}
	p, _ := digestOf([]Value{vText("a"), vBlob('a')}, nil)
	q, _ := digestOf([]Value{vBlob('a'), vText("a")}, nil)
	if p == q {
		t.Error("the digest is not order-sensitive")
	}
	if _, c := digestOf([]Value{vOmitted(vText("ab"))}, nil); c {
		t.Error("an omitted value leaves the digest complete")
	}
	if _, c := digestOf([]Value{vClipped(vText("ab"), 1)}, nil); c {
		t.Error("a clipped value leaves the digest complete")
	}
	if _, c := digestOf([]Value{vInt(1), vOmitted(vText("ab"))}, []int{0}); !c {
		t.Error("an omitted value outside the key makes the key digest incomplete")
	}
	if _, c := digestOf([]Value{vInt(1), vOmitted(vText("ab"))}, []int{1}); c {
		t.Error("an omitted key value leaves the key digest complete")
	}
}

// TestLiveDamagedCodes (review M-5): each structural-damage warning code makes
// the live digest set suspect; other codes do not.
func TestLiveDamagedCodes(t *testing.T) {
	damaged := []string{
		WarnPageUnavailable, WarnPageTypeInvalid, WarnPageRange, WarnCellPointer, WarnCellOverflowChain,
		WarnCellTooLarge, WarnRecordInvalid, WarnBTreeCycle, WarnBTreeDepth, WarnBTreeOrder, WarnBTreeShape,
		WarnSchemaRowInvalid, WarnLimitReached,
	}
	for _, c := range damaged {
		v := &View{warns: newWarnings(10)}
		v.warns.add(Warning{Code: c, Msg: "x"})
		if !liveDamaged(v) {
			t.Errorf("warning %s does not mark the live scan as damaged", c)
		}
	}
	for _, c := range []string{WarnFreelistCount, WarnWALTornTail, WarnJournalHot, WarnHdrFractions} {
		v := &View{warns: newWarnings(10)}
		v.warns.add(Warning{Code: c, Msg: "x"})
		if liveDamaged(v) {
			t.Errorf("warning %s marks the live scan as damaged", c)
		}
	}
	if liveDamaged(&View{warns: newWarnings(10)}) {
		t.Error("a view without warnings is damaged")
	}
}

// TestIsAnswerless (review M-1): the errors that only mean "the live state cannot
// answer" and the ones that end the pass.
func TestIsAnswerless(t *testing.T) {
	for _, e := range []error{ErrNotFound, ErrCorrupt, ErrLimit, ErrPageUnavailable, ErrWithoutRowid} {
		if !isAnswerless(fmt.Errorf("wrapped: %w", e)) {
			t.Errorf("%v is not answerless", e)
		}
	}
	for _, e := range []error{context.Canceled, errors.New("io failure"), ErrInternal} {
		if isAnswerless(e) {
			t.Errorf("%v is answerless", e)
		}
	}
}
