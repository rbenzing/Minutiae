package plist

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	howett "howett.net/plist"

	"github.com/rbenzing/minutiae/internal/parse"
)

// recBudget is a Budget that records what is charged, so a test reads the estimate instead of
// recomputing it.
type recBudget struct{ sum int64 }

func (r *recBudget) Alloc(n int64) error { r.sum += n; return nil }
func (r *recBudget) Free(n int64)        { r.sum -= n }

// chargeShape is one document shape, built in the test, in the formats it exists in.
type chargeShape struct {
	name   string
	binary func() []byte // nil when the shape has no binary form
	xml    func() []byte // nil when the shape has no XML form
}

func marshalBoth(v func() any) (func() []byte, func() []byte) {
	enc := func(format int) func() []byte {
		return func() []byte {
			b, err := howett.Marshal(v(), format)
			if err != nil {
				panic(err)
			}
			return b
		}
	}
	return enc(howett.BinaryFormat), enc(howett.XMLFormat)
}

func both(name string, v func() any) chargeShape {
	bin, x := marshalBoth(v)
	return chargeShape{name: name, binary: bin, xml: x}
}

func repeatSlice(n int, f func(i int) any) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

// nestedDicts is a chain of depth nested single-key dictionaries.
func nestedDicts(depth int) any {
	var v any = int64(1)
	for range depth {
		v = map[string]any{"k": v}
	}
	return v
}

func chargeShapes() []chargeShape {
	return []chargeShape{
		both("flat", func() any { return repeatSlice(100000, func(i int) any { return int64(i) }) }),
		both("maps", func() any {
			return repeatSlice(50000, func(i int) any {
				return map[string]any{"a": int64(i), "b": "x", "c": true}
			})
		}),
		both("bigmap", func() any {
			m := make(map[string]any, 100000)
			for i := range 100000 {
				m["key"+strconv.Itoa(i)] = int64(i)
			}
			return m
		}),
		both("strings", func() any {
			return repeatSlice(1000, func(i int) any { return strings.Repeat("s", 4096) + strconv.Itoa(i) })
		}),
		both("data", func() any {
			return repeatSlice(1000, func(int) any { return make([]byte, 4096) })
		}),
		both("emptydicts", func() any {
			return repeatSlice(300000, func(int) any { return map[string]any{} })
		}),
		both("emptyarrays", func() any {
			return repeatSlice(300000, func(int) any { return []any{} })
		}),
		both("onekeydicts", func() any {
			return repeatSlice(150000, func(i int) any { return map[string]any{"k": int64(i)} })
		}),
		both("twokeydicts", func() any {
			return repeatSlice(100000, func(i int) any { return map[string]any{"a": int64(i), "b": int64(i)} })
		}),
		both("nested30", func() any {
			return repeatSlice(5000, func(int) any { return nestedDicts(30) })
		}),
		both("nested60", func() any {
			return repeatSlice(2500, func(int) any { return nestedDicts(60) })
		}),
		both("bools", func() any { return repeatSlice(300000, func(i int) any { return i%2 == 0 }) }),
		both("dates", func() any {
			return repeatSlice(100000, func(i int) any { return time.Unix(int64(1_600_000_000+i), 0).UTC() })
		}),
		both("shortstrings", func() any {
			return repeatSlice(200000, func(i int) any { return "s" + strconv.Itoa(i%10) })
		}),
		both("longkeys", func() any {
			return repeatSlice(100, func(i int) any {
				m := make(map[string]any, 100)
				for j := range 100 {
					m[strings.Repeat("k", 1000)+strconv.Itoa(i*1000+j)] = true
				}
				return m
			})
		}),
		both("longstrings", func() any {
			return repeatSlice(100, func(i int) any { return strings.Repeat("l", 100*1024) + strconv.Itoa(i) })
		}),
		{
			name: "attrstrings",
			xml: func() []byte {
				attr := ` a="` + strings.Repeat("v", 200) + `"`
				return []byte(`<plist version="1.0"><array>` +
					strings.Repeat(`<string`+attr+`>x</string>`, 100000) + `</array></plist>`)
			},
		},
		{
			name: "emptydictsself",
			xml: func() []byte {
				return []byte(`<plist version="1.0"><array>` + strings.Repeat(`<dict/>`, 300000) + `</array></plist>`)
			},
		},
	}
}

// measureCharge decodes b with a recording budget and returns the charged estimate and the
// bytes allocated by the call (TotalAlloc is cumulative, so GC timing does not matter).
func measureCharge(t *testing.T, b []byte) (charged, allocated int64) {
	t.Helper()
	rec := &recBudget{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := DecodeReport(b, rec)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("DecodeReport: %v", err)
	}
	return rec.sum, int64(after.TotalAlloc - before.TotalAlloc)
}

// TestChargeCoversMeasuredAllocation holds the memory charge to a measurement: on every shape
// and format the estimate charged before decoding is at least the bytes allocated while
// decoding. An over-charge is allowed (it fails closed); an under-charge is the failure.
func TestChargeCoversMeasuredAllocation(t *testing.T) {
	for _, s := range chargeShapes() {
		for _, f := range []struct {
			name  string
			build func() []byte
		}{{"binary", s.binary}, {"xml", s.xml}} {
			if f.build == nil {
				continue
			}
			t.Run(s.name+"/"+f.name, func(t *testing.T) {
				b := f.build()
				nodes, payload, err := measure(b, DefaultLimits())
				if err != nil {
					t.Fatalf("measure: %v", err)
				}
				charged, allocated := measureCharge(t, b)
				t.Logf("CHARGE shape=%s format=%s len=%d nodes=%d payload=%d charged=%d allocated=%d ratio=%.3f",
					s.name, f.name, len(b), nodes, payload, charged, allocated, float64(allocated)/float64(charged))
				if charged < allocated {
					t.Errorf("under-charge: charged %d, allocated %d", charged, allocated)
				}
			})
		}
	}
}

// TestMaxDocumentChargeIsRefusedNotCut pins ruling V51. The charge is held to the measurement,
// so a document at the caps is charged more than the default memory budget: such a document is
// refused with ErrNoBudget before the library allocates anything, never partly decoded and
// never silently cut. The product limits (MemBudget, MaxInput, MaxNodes) are not changed.
func TestMaxDocumentChargeIsRefusedNotCut(t *testing.T) {
	budget := parse.DefaultLimits().MemBudget
	l := DefaultLimits()
	atCaps := int64(MaxInput)*chargePerInputByte + int64(l.MaxNodes)*chargePerNode + int64(l.MaxPayload)*chargePerPayload
	atCapsXML := atCaps + int64(l.MaxNodes)*chargePerXMLNode
	t.Logf("CAP binary=%d xml=%d budget=%d (%.2f and %.2f GiB)", atCaps, atCapsXML, budget,
		float64(atCaps)/(1<<30), float64(atCapsXML)/(1<<30))
	if atCaps <= budget || atCapsXML <= budget {
		t.Fatalf("the pin is vacuous: a document at the caps (binary %d, XML %d) fits the budget %d", atCaps, atCapsXML, budget)
	}

	// A real XML document beyond the default budget: 600000 empty dictionaries.
	b := []byte(`<plist version="1.0"><array>` + strings.Repeat(`<dict/>`, 600000) + `</array></plist>`)
	nodes, payload, err := measure(b, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	estimate := int64(len(b))*chargePerInputByte + int64(nodes)*(chargePerNode+chargePerXMLNode) + int64(payload)*chargePerPayload
	if estimate <= budget {
		t.Fatalf("the test document is charged %d, which fits the budget %d", estimate, budget)
	}
	for name, call := range map[string]func(Budget) (any, error){
		"Decode":       func(bu Budget) (any, error) { return Decode(b, bu) },
		"DecodeReport": func(bu Budget) (any, error) { v, _, err := DecodeReport(b, bu); return v, err },
	} {
		view := newView(budget)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		v, err := call(view)
		runtime.ReadMemStats(&after)
		if v != nil || !errors.Is(err, ErrNoBudget) {
			t.Fatalf("%s: value %v, err %v; want nil and ErrNoBudget", name, v != nil, err)
		}
		if view.Used() != 0 {
			t.Fatalf("%s: the refused document keeps %d bytes charged", name, view.Used())
		}
		delta := int64(after.TotalAlloc - before.TotalAlloc)
		t.Logf("%s refused: %d bytes allocated for a charge of %d", name, delta, estimate)
		if delta > estimate/100 {
			t.Fatalf("%s: %d bytes allocated before the refusal, more than 1%% of the %d charged", name, delta, estimate)
		}
	}

	// The edge: a budget of exactly the estimate accepts a binary document, one byte less refuses it.
	bin := marshalAs(t, repeatSlice(1000, func(i int) any { return int64(i) }), howett.BinaryFormat)
	bn, bp, err := measure(bin, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	need := int64(len(bin))*chargePerInputByte + int64(bn)*chargePerNode + int64(bp)*chargePerPayload
	if _, _, err := DecodeReport(bin, newView(need)); err != nil {
		t.Fatalf("a budget of exactly the estimate (%d): %v", need, err)
	}
	if _, _, err := DecodeReport(bin, newView(need-1)); !errors.Is(err, ErrNoBudget) {
		t.Fatalf("a budget one byte short: err %v, want ErrNoBudget", err)
	}
}
