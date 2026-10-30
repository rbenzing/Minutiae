package carve

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

const boundedHdr = "BNDHDR!!"

// boundedValidators adds validators that answer Bounded in the ways the
// scanner must tell apart: no assumption (window-bounded or not) and an
// assumption of their own.
func boundedValidators() map[string]validatorFunc {
	v := testValidators()
	v["b-none"] = func(w *Window) Verdict { return Verdict{Outcome: Bounded, Length: w.Len()} }
	v["b-set"] = func(w *Window) Verdict {
		return Verdict{Outcome: Bounded, Length: w.Len(), Assumptions: []string{"validator-said-so"}}
	}
	v["b-short"] = func(w *Window) Verdict { return Verdict{Outcome: Bounded, Length: w.Len() - 1} }
	v["b-short-set"] = func(w *Window) Verdict {
		return Verdict{Outcome: Bounded, Length: w.Len() - 1, Assumptions: []string{"validator-said-so"}}
	}
	return v
}

func boundedType(validate string, maxLen int64) Type {
	t := toyType("bnd", boundedHdr, validate)
	t.MaxLen = maxLen
	return t
}

func scanBounded(t *testing.T, img []byte, runs []Run, set *Set, o Options) ([]Hit, Stats) {
	t.Helper()
	var hits []Hit
	st, err := scan(context.Background(), bytes.NewReader(img), runs, set, o, boundedValidators(), func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits, st
}

// TestBoundedValidatorVerdictKeepsItsAssumption: an assumption the validator
// states is never replaced or extended by the scanner.
func TestBoundedValidatorVerdictKeepsItsAssumption(t *testing.T) {
	img := carvetest.Image(4*kib, fillPattern)
	carvetest.Place(img, 1024, []byte(boundedHdr))
	hits, _ := scanBounded(t, img, whole(img), toySet(boundedType("b-set", 512)), Options{Align: 1})
	if len(hits) != 1 || hits[0].Outcome != Bounded || hits[0].Length != 512 ||
		!slices.Equal(hits[0].Assumptions, []string{"validator-said-so"}) {
		t.Fatalf("hits = %+v, want one bounded hit keeping only the validator's assumption", hits)
	}
}

// TestBoundedWithoutAssumptionIsNamedByTheScanner: one case per label.
func TestBoundedWithoutAssumptionIsNamedByTheScanner(t *testing.T) {
	t.Run("length-capped", func(t *testing.T) {
		img := carvetest.Image(4*kib, fillPattern)
		carvetest.Place(img, 1024, []byte(boundedHdr))
		hits, _ := scanBounded(t, img, whole(img), toySet(boundedType("b-none", 512)), Options{Align: 1})
		if len(hits) != 1 || hits[0].Length != 512 || !slices.Equal(hits[0].Assumptions, []string{"length-capped"}) {
			t.Fatalf("hits = %+v", hits)
		}
	})
	t.Run("cut-at-run-end", func(t *testing.T) {
		img := carvetest.Image(4*kib, fillPattern)
		carvetest.Place(img, 1024, []byte(boundedHdr))
		hits, _ := scanBounded(t, img, []Run{{0, 1324}}, toySet(boundedType("b-none", 4096)), Options{Align: 1})
		if len(hits) != 1 || hits[0].Length != 300 || !slices.Equal(hits[0].Assumptions, []string{"cut-at-run-end"}) {
			t.Fatalf("hits = %+v", hits)
		}
	})
	t.Run("cut-at-outer-object-end", func(t *testing.T) {
		img := carvetest.Image(4*kib, fillPattern)
		outer := toyObj(toyHdr, 200)
		copy(outer[150:], boundedHdr)
		carvetest.Place(img, 1024, outer)
		set := toySet(toyType("toy", toyHdr, "test"), boundedType("b-none", 4096))
		hits, _ := scanBounded(t, img, whole(img), set, Options{Align: 1, Nested: true})
		var inner []Hit
		for _, h := range hits {
			if h.Family == "bnd" {
				inner = append(inner, h)
			}
		}
		if len(inner) != 1 || inner[0].Length != 50 || inner[0].NestedIn != 1024 ||
			!slices.Contains(inner[0].Assumptions, "cut-at-outer-object-end") {
			t.Fatalf("inner hits = %+v", inner)
		}
	})
}

// TestBoundedVerdictNeverLacksAnAssumption: a Bounded verdict whose length is
// not the window end cannot be given a label, so it is rejected with a typed,
// counted reason; with an assumption of its own it is kept.
func TestBoundedVerdictNeverLacksAnAssumption(t *testing.T) {
	img := carvetest.Image(4*kib, fillPattern)
	carvetest.Place(img, 1024, []byte(boundedHdr))
	hits, st := scanBounded(t, img, whole(img), toySet(boundedType("b-short", 512)), Options{Align: 1})
	if len(hits) != 0 || st.Rejected["validator-defect"] != 1 || st.Accepted[Bounded] != 0 {
		t.Fatalf("hits = %+v rejected = %v accepted = %v, want one validator-defect and no hit", hits, st.Rejected, st.Accepted)
	}
	hits, st = scanBounded(t, img, whole(img), toySet(boundedType("b-short-set", 512)), Options{Align: 1})
	if len(hits) != 1 || hits[0].Length != 511 || !slices.Equal(hits[0].Assumptions, []string{"validator-said-so"}) || len(st.Rejected) != 0 {
		t.Fatalf("hits = %+v rejected = %v", hits, st.Rejected)
	}
}
