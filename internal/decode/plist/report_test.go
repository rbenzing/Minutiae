package plist

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"runtime"
	"testing"

	howett "howett.net/plist"
)

// dupDictBinary is {"a": 1, "a": 2}: object 0 the dict with two pairs (key refs 1 and 1, value
// refs 2 and 3), object 1 the key "a", objects 2 and 3 the integers.
func dupDictBinary() []byte {
	return objectsPlist(
		[]byte{0xD2, 1, 1, 2, 3},
		[]byte{0x51, 'a'},
		[]byte{0x10, 1},
		[]byte{0x10, 2},
	)
}

// bigDupDict is a binary plist with offset size 4 and reference size 1 holding one dict of n
// pairs whose keys all refer to one string object and whose values all refer to one integer.
func bigDupDict(n uint64) []byte {
	b := []byte("bplist00")
	var offsets []byte
	offsets = binary.BigEndian.AppendUint32(offsets, uint32(len(b)))
	b = append(b, bigCount(0xD, n)...)
	for range n {
		b = append(b, 1)
	}
	for range n {
		b = append(b, 2)
	}
	offsets = binary.BigEndian.AppendUint32(offsets, uint32(len(b)))
	b = append(b, 0x51, 'a')
	offsets = binary.BigEndian.AppendUint32(offsets, uint32(len(b)))
	b = append(b, 0x10, 1)
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 4, 1
	binary.BigEndian.PutUint64(trailer[8:], 3)
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

func TestDecodeReportCountsBinaryDuplicateKeys(t *testing.T) {
	v, rep, err := DecodeReport(dupDictBinary(), newView(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"a": int64(2)}; !reflect.DeepEqual(v, want) {
		t.Fatalf("value %#v, want %#v", v, want)
	}
	if rep != (Report{DuplicateKeys: 1}) || rep.Lossless() {
		t.Fatalf("report %+v, lossless %v", rep, rep.Lossless())
	}
}

func TestDecodeReportIsLosslessWithoutDuplicates(t *testing.T) {
	b := marshalAs(t, map[string]any{"a": 1, "b": []any{map[string]any{"c": 2}}}, howett.BinaryFormat)
	_, rep, err := DecodeReport(b, newView(1<<20))
	if err != nil || rep != (Report{}) || !rep.Lossless() {
		t.Fatalf("report %+v lossless %v err %v", rep, rep.Lossless(), err)
	}
}

func TestDecodeStrictRefusesDuplicateKeys(t *testing.T) {
	view := newView(1 << 20)
	v, err := Decode(dupDictBinary(), view)
	if v != nil || err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Decode = %#v, %v; want nil and ErrUnsupported", v, err)
	}
	var le *LossError
	if !errors.As(err, &le) || le.Report != (Report{DuplicateKeys: 1}) {
		t.Fatalf("error %v: not a *LossError with one duplicate", err)
	}
	if !typedErr(err, false) {
		t.Fatalf("error %v is not typed", err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused document keeps %d bytes charged", view.Used())
	}
}

// Passes on arrival: Check refuses the document before any duplicate is counted, so both entry
// points return the malformed sentinel. It pins the binary half of the rule; the mutation that
// returns the *LossError before the library runs must fail it.
func TestDecodeStrictAndReportAgreeOnMalformedDocuments(t *testing.T) {
	b := objectsPlist(
		[]byte{0xD2, 1, 1, 2, 9}, // value ref 9 is out of range
		[]byte{0x51, 'a'},
		[]byte{0x10, 1},
	)
	_, err1 := Decode(b, newView(1<<20))
	_, _, err2 := DecodeReport(b, newView(1<<20))
	if !errors.Is(err1, ErrMalformed) || !errors.Is(err2, ErrMalformed) {
		t.Fatalf("Decode: %v; DecodeReport: %v; want ErrMalformed from both", err1, err2)
	}
	var le *LossError
	if errors.As(err1, &le) {
		t.Fatalf("a malformed document returned a *LossError")
	}
}

func TestDecodeReportCountsDuplicatesAcrossNesting(t *testing.T) {
	// {"k": 5, "k": [ {"x":1, "x":2, "x":3} ]}: the surviving value holds the inner dict.
	b := objectsPlist(
		[]byte{0xD2, 1, 1, 5, 2},
		[]byte{0x51, 'k'},
		refArray(3),
		[]byte{0xD3, 4, 4, 4, 6, 7, 8},
		[]byte{0x51, 'x'},
		[]byte{0x10, 5},
		[]byte{0x10, 1},
		[]byte{0x10, 2},
		[]byte{0x10, 3},
	)
	v, rep, err := DecodeReport(b, newView(1<<20))
	if err != nil || rep.DuplicateKeys != 3 {
		t.Fatalf("report %+v err %v; want 3 (two inner, one outer)", rep, err)
	}
	if want := map[string]any{"k": []any{map[string]any{"x": int64(3)}}}; !reflect.DeepEqual(v, want) {
		t.Fatalf("value %#v, want %#v", v, want)
	}
	// {"k": [ {x,x,x} ], "k": 5}: the dict with the inner duplicates is itself the replaced
	// entry and is not walked, so only the outer duplicate is counted.
	b = objectsPlist(
		[]byte{0xD2, 1, 1, 2, 5},
		[]byte{0x51, 'k'},
		refArray(3),
		[]byte{0xD3, 4, 4, 4, 6, 7, 8},
		[]byte{0x51, 'x'},
		[]byte{0x10, 5},
		[]byte{0x10, 1},
		[]byte{0x10, 2},
		[]byte{0x10, 3},
	)
	if _, rep, err = DecodeReport(b, newView(1<<20)); err != nil || rep.DuplicateKeys != 1 {
		t.Fatalf("replaced entry: report %+v err %v; want 1", rep, err)
	}
}

func TestDecodeReportASCIIAndUTF16KeysAreOneKey(t *testing.T) {
	b := objectsPlist(
		[]byte{0xD2, 1, 2, 3, 4},
		[]byte{0x51, 'a'},
		[]byte{0x61, 0x00, 'a'},
		[]byte{0x10, 1},
		[]byte{0x10, 2},
	)
	v, rep, err := DecodeReport(b, newView(1<<20))
	if err != nil || rep.DuplicateKeys != 1 {
		t.Fatalf("report %+v err %v; want 1", rep, err)
	}
	if want := map[string]any{"a": int64(2)}; !reflect.DeepEqual(v, want) {
		t.Fatalf("value %#v, want %#v", v, want)
	}
}

func TestDecodeReportSharedDictCountsEveryOccurrence(t *testing.T) {
	b := objectsPlist(
		refArray(1, 1),
		[]byte{0xD2, 2, 2, 3, 4},
		[]byte{0x51, 'a'},
		[]byte{0x10, 1},
		[]byte{0x10, 2},
	)
	_, rep, err := DecodeReport(b, newView(1<<20))
	if err != nil || rep.DuplicateKeys != 2 {
		t.Fatalf("report %+v err %v; want 2", rep, err)
	}
}

func TestDecodeReportDuplicateKeyBombIsBounded(t *testing.T) {
	const n = 100000
	b := bigDupDict(n)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	bombView := newView(1 << 30)
	v, rep, err := DecodeReport(b, bombView)
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
	delta := after.TotalAlloc - before.TotalAlloc
	// The honesty claim: what the call allocated stays within what it charged the budget. First
	// green run: 5278016 bytes allocated against a charge of about 14 MB.
	charged := uint64(bombView.Used())
	t.Logf("TotalAlloc delta %d, charged %d", delta, charged)
	if delta > charged {
		t.Fatalf("allocated %d bytes but charged only %d", delta, charged)
	}

	view := newView(1 << 30)
	huge := bigDupDict(2_000_000)
	if _, _, err := DecodeReport(huge, view); !errors.Is(err, ErrLimit) {
		t.Fatalf("2M pairs: err %v, want ErrLimit", err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused document keeps %d bytes charged", view.Used())
	}
}

func TestFuzzSeedsCoverSetsAndDuplicateKeys(t *testing.T) {
	loss := 0
	for _, s := range decodeSeeds() {
		_, err := Decode(s, newView(256<<20))
		var le *LossError
		if errors.As(err, &le) {
			loss++
		}
	}
	if loss == 0 {
		t.Fatalf("no decode seed makes the strict Decode return a *LossError")
	}
}

func TestReportPlusSaturates(t *testing.T) {
	a := Report{DuplicateKeys: math.MaxUint64 - 1}
	if got := a.Plus(Report{DuplicateKeys: 5}); got.DuplicateKeys != math.MaxUint64 {
		t.Fatalf("Plus = %d, want saturation", got.DuplicateKeys)
	}
	if got := (Report{DuplicateKeys: 2}).Plus(Report{DuplicateKeys: 3}); got.DuplicateKeys != 5 {
		t.Fatalf("Plus = %d, want 5", got.DuplicateKeys)
	}
	if (Report{DuplicateKeys: 1}).Lossless() || !(Report{}).Lossless() {
		t.Fatal("Lossless")
	}
}

// strictLenientContract holds the strict and the lenient entry point to the rule of the
// package: when the strict one succeeds the lenient one succeeds with an equal value and a
// lossless report; when the strict one refuses with a *LossError the lenient one succeeds and
// reports the same loss; in every other case both fail with the same sentinel.
func strictLenientContract(t *testing.T, b []byte, strict func([]byte, Budget) (any, error), lenient func([]byte, Budget) (any, Report, error)) {
	t.Helper()
	sv, serr := strict(b, newView(256<<20))
	lv, rep, lerr := lenient(b, newView(256<<20))
	var le *LossError
	switch {
	case serr == nil:
		if lerr != nil || !rep.Lossless() || !sameValue(sv, lv) {
			t.Fatalf("strict succeeded but lenient gave %v, %+v, %v", lv, rep, lerr)
		}
	case errors.As(serr, &le):
		if lerr != nil || le.Report.DuplicateKeys == 0 || rep != le.Report {
			t.Fatalf("strict refused with %+v but lenient gave %+v, %v", le.Report, rep, lerr)
		}
	default:
		if lerr == nil {
			t.Fatalf("strict failed (%v) but lenient succeeded", serr)
		}
		for _, s := range []error{ErrMalformed, ErrLimit, ErrUnsupported, ErrNoBudget, ErrInternal} {
			if errors.Is(serr, s) != errors.Is(lerr, s) {
				t.Fatalf("different sentinels: strict %v, lenient %v", serr, lerr)
			}
		}
	}
}
