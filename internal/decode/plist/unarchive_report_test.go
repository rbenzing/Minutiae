package plist

import (
	"errors"
	"math"
	"reflect"
	"runtime"
	"testing"
)

// dupDictArchive is an archive whose root is an NSDictionary that repeats the key "k".
func dupDictArchive() map[string]any {
	return archive(1,
		map[string]any{"$class": UID(2), "NS.keys": []any{UID(3), UID(3)}, "NS.objects": []any{UID(4), UID(5)}},
		classRec("NSDictionary"), "k", "first", "second")
}

func TestUnarchiveReportCountsDuplicateDictKeys(t *testing.T) {
	out, rep, err := UnarchiveReport(dupDictArchive(), newView(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"k": "second"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("value %#v, want %#v", out, want)
	}
	if rep != (Report{DuplicateKeys: 1}) {
		t.Fatalf("report %+v, want one duplicate", rep)
	}
	view := newView(1 << 30)
	got, err := Unarchive(dupDictArchive(), view)
	var le *LossError
	if got != nil || !errors.Is(err, ErrUnsupported) || !errors.As(err, &le) || le.Report != (Report{DuplicateKeys: 1}) {
		t.Fatalf("Unarchive = %#v, %v; want nil and a *LossError with one duplicate", got, err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused archive keeps %d bytes charged", view.Used())
	}
}

func TestUnarchiveReportCountsASharedDictionaryOnce(t *testing.T) {
	a := archive(1,
		map[string]any{"$class": UID(2), "NS.objects": []any{UID(4), UID(4)}},
		classRec("NSArray"),
		classRec("NSDictionary"),
		map[string]any{"$class": UID(3), "NS.keys": []any{UID(5), UID(5)}, "NS.objects": []any{UID(6), UID(7)}},
		"k", "first", "second")
	out, rep, err := UnarchiveReport(a, newView(1<<30))
	if err != nil || rep != (Report{DuplicateKeys: 1}) {
		t.Fatalf("report %+v err %v; want 1 (a shared object is resolved once)", rep, err)
	}
	d := map[string]any{"k": "second"}
	if want := []any{d, d}; !reflect.DeepEqual(out, want) {
		t.Fatalf("value %#v, want %#v", out, want)
	}
}

// bigDupDictArchive is an NSDictionary of n identical keys and n identical values.
func bigDupDictArchive(n int) map[string]any {
	keys, vals := make([]any, n), make([]any, n)
	for i := range n {
		keys[i], vals[i] = UID(3), UID(4)
	}
	return archive(1,
		map[string]any{"$class": UID(2), "NS.keys": keys, "NS.objects": vals},
		classRec("NSDictionary"), "k", "v")
}

func TestUnarchiveReportDuplicateKeyBombIsBounded(t *testing.T) {
	const n = 100000
	a := bigDupDictArchive(n)
	view := newView(1 << 30)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, rep, err := UnarchiveReport(a, view)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"k": "v"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("value %#v, want %#v", out, want)
	}
	if rep.DuplicateKeys != n-1 {
		t.Fatalf("DuplicateKeys = %d, want %d", rep.DuplicateKeys, n-1)
	}
	delta, charged := int64(after.TotalAlloc-before.TotalAlloc), view.Used()
	t.Logf("TotalAlloc delta %d, charged %d", delta, charged)
	if delta > charged {
		t.Fatalf("allocated %d bytes but charged only %d", delta, charged)
	}

	hv := newView(1 << 30)
	if _, _, err := UnarchiveReport(bigDupDictArchive(2_000_000), hv); !errors.Is(err, ErrLimit) {
		t.Fatalf("2M keys: err %v, want ErrLimit", err)
	}
	if hv.Used() != 0 {
		t.Fatalf("the refused archive keeps %d bytes charged", hv.Used())
	}
}

func TestUnarchiveSetsKeepSetness(t *testing.T) {
	for _, c := range []struct {
		class string
		want  any
	}{
		{"NSSet", Set{Items: []any{"a", "b"}}},
		{"NSMutableSet", Set{Items: []any{"a", "b"}}},
		{"NSOrderedSet", Set{Items: []any{"a", "b"}, Ordered: true}},
		{"NSMutableOrderedSet", Set{Items: []any{"a", "b"}, Ordered: true}},
		{"NSArray", []any{"a", "b"}},
		{"NSMutableArray", []any{"a", "b"}},
	} {
		a := archive(1,
			map[string]any{"$class": UID(2), "NS.objects": []any{UID(3), UID(4)}},
			classRec(c.class), "a", "b")
		if got := unarchive(t, a); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, want %#v", c.class, got, c.want)
		}
	}
	// Items stay in archive order and uniqueness is not checked.
	a := archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{UID(3), UID(4), UID(3)}},
		classRec("NSSet"), "z", "a")
	if got, want := unarchive(t, a), (Set{Items: []any{"z", "a", "z"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("order: %#v, want %#v", got, want)
	}
}

func TestUnarchiveNSDateKeepsStoredNumberType(t *testing.T) {
	for _, c := range []struct {
		name   string
		stored any
	}{
		{"float64", float64(7.5e8)},
		{"int64 above 2^53", int64(1<<53 + 1)},
		{"uint64 max", uint64(math.MaxUint64)},
		{"negative int64", int64(-5)},
	} {
		a := archive(1, map[string]any{"$class": UID(2), "NS.time": c.stored}, classRec("NSDate"))
		want := map[string]any{"$class": "NSDate", "NS.time": c.stored}
		got := unarchive(t, a)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %#v, want %#v", c.name, got, want)
		}
	}
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.time": "x"}, classRec("NSDate")), ErrMalformed)
}

func TestUnarchiveStrictAndReportAgreeOnMalformedArchives(t *testing.T) {
	a := dupDictArchive()
	a["$objects"] = append(a["$objects"].([]any), map[string]any{"$class": UID(2), "NS.keys": []any{UID(1)}, "NS.objects": []any{}})
	a["$top"] = map[string]any{"a": UID(1), "b": UID(6)}
	view1, view2 := newView(1<<30), newView(1<<30)
	_, err1 := Unarchive(a, view1)
	_, _, err2 := UnarchiveReport(a, view2)
	if !errors.Is(err1, ErrMalformed) || !errors.Is(err2, ErrMalformed) {
		t.Fatalf("Unarchive: %v; UnarchiveReport: %v; want ErrMalformed from both", err1, err2)
	}
	if view1.Used() != 0 || view2.Used() != 0 {
		t.Fatalf("charges kept: %d and %d", view1.Used(), view2.Used())
	}
}

// setArchive is an archive whose root is a set of the given class.
func setArchive(class string) map[string]any {
	return archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{UID(3), UID(4)}}, classRec(class), "a", "b")
}

// unarchiveStrictLenient decodes b and holds Unarchive and UnarchiveReport to the same
// strict-versus-lenient rule as Decode and DecodeReport (strictLenientContract).
func unarchiveStrictLenient(t *testing.T, b []byte) {
	t.Helper()
	decode := func(b []byte) (any, error) {
		v, _, err := DecodeReport(b, newView(256<<20))
		return v, err
	}
	strictLenientContract(t, b,
		func(b []byte, bu Budget) (any, error) {
			v, err := decode(b)
			if err != nil {
				return nil, err
			}
			return Unarchive(v, bu)
		},
		func(b []byte, bu Budget) (any, Report, error) {
			v, err := decode(b)
			if err != nil {
				return nil, Report{}, err
			}
			return UnarchiveReport(v, bu)
		})
}

// TestFuzzSeedsCoverArchiveSetsAndDuplicateKeys: the archive seeds really exercise the new
// paths under plain `go test`: at least one unarchives to a Set and one makes the strict
// Unarchive return a *LossError.
func TestFuzzSeedsCoverArchiveSetsAndDuplicateKeys(t *testing.T) {
	sets, loss := 0, 0
	for _, s := range archiveSeeds() {
		v, _, err := DecodeReport(s, newView(256<<20))
		if err != nil {
			continue
		}
		if out, _, err := UnarchiveReport(v, newView(256<<20)); err == nil {
			if _, isSet := out.(Set); isSet {
				sets++
			}
		}
		var le *LossError
		if _, err := Unarchive(v, newView(256<<20)); errors.As(err, &le) {
			loss++
		}
	}
	if sets == 0 || loss == 0 {
		t.Fatalf("archive seeds: %d unarchive to a Set, %d to a *LossError; want both", sets, loss)
	}
}
