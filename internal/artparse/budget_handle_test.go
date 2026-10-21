package artparse_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
)

// TestInvocationsDoNotShareABudgetView: each Input of a bundle, the Probe
// input included, carries its own BudgetView onto its own accounting. A parser
// that overwrites its handle detaches only that handle.
func TestInvocationsDoNotShareABudgetView(t *testing.T) {
	f := newFixture(t, true, artparse.Options{})
	b := f.open(t)
	defer b.Close()

	in1, in2, probe := b.Input(false), b.Input(false), b.Input(true)
	if in1.Budget == in2.Budget || in1.Budget == probe.Budget || in2.Budget == probe.Budget {
		t.Fatal("two inputs share a BudgetView pointer")
	}
	if err := in1.Budget.Alloc(100); err != nil {
		t.Fatal(err)
	}
	*in1.Budget = parse.BudgetView{}
	if err := in1.Budget.Alloc(1); err == nil {
		t.Fatal("the overwritten handle must fail closed")
	}
	for name, in := range map[string]*parse.Input{"second input": in2, "probe input": probe} {
		if in.Budget.Used() != 0 {
			t.Errorf("%s sees used = %d from another invocation", name, in.Budget.Used())
		}
		if err := in.Budget.Alloc(10); err != nil {
			t.Errorf("%s cannot allocate: %v", name, err)
		}
		if in.Budget.Used() != 10 || in.Budget.Limit() == 0 {
			t.Errorf("%s used/limit = %d/%d, want 10 and the job limit", name, in.Budget.Used(), in.Budget.Limit())
		}
	}
}

// TestInputLookupIsNotNameableByAParser is a documentation pin, not a
// defence: the only implementation of parse.Lookuper a parser can receive is
// a pointer to an unexported struct of this package, so it cannot be named,
// and the purity rules forbid reflect and unsafe in parsers.
func TestInputLookupIsNotNameableByAParser(t *testing.T) {
	f := newFixture(t, true, artparse.Options{})
	b := f.open(t)
	defer b.Close()
	typ := reflect.TypeOf(b.Input(false).Lookup)
	if typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		t.Fatalf("Lookup has dynamic type %v, want a pointer to a struct", typ)
	}
	el := typ.Elem()
	if el.Name() == "" || el.Name()[0] >= 'A' && el.Name()[0] <= 'Z' {
		t.Errorf("the Lookuper type name %q is exported", el.Name())
	}
	if !strings.HasSuffix(el.PkgPath(), "/internal/artparse") {
		t.Errorf("the Lookuper type lives in %q, want .../internal/artparse", el.PkgPath())
	}
}
