package sqlitefile

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fitLivePass builds a rowPass over a database whose rowid table t holds the
// live row 1 = (7, "live") and WITHOUT ROWID table w holds ("k", "v").
func fitLivePass(t *testing.T) *rowPass {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(a, b)").Insert(1, int64(7), "live")
	b.CreateTableWithoutRowid("w", "create table w(k primary key, v) without rowid", 1).Insert(1, "k", "v")
	db := b.Bytes()
	d, err := Open(bytes.NewReader(db), int64(len(db)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	l := h.d.env.newLedger()
	t.Cleanup(func() { l.free(l.n) })
	return &rowPass{h: h, ctx: context.Background(), l: l, tables: map[string]*Table{}, wr: map[string]*wrSet{}, limits: map[string]bool{}}
}

func rowidRow(id int64, vals ...Value) *RecoveredRow {
	return &RecoveredRow{Rowid: &id, Values: vals}
}

func TestEqualsLiveFitDecision(t *testing.T) {
	one := ident{table: "t", basis: BasisFit, kind: kindRowid}
	for _, c := range []struct {
		name string
		id   ident
		row  *RecoveredRow
		want bool
	}{
		{"equal", one, rowidRow(1, vInt(7), vText("live")), true},
		{"another value", one, rowidRow(1, vInt(8), vText("live")), false},
		{"another storage kind", one, rowidRow(1, vText("7"), vText("live")), false},
		{"another rowid", one, rowidRow(2, vInt(7), vText("live")), false},
		{"no rowid", one, &RecoveredRow{Values: []Value{vInt(7), vText("live")}}, false},
		{"no object proven", ident{table: "t", basis: BasisFit, kind: kindNone}, rowidRow(1, vInt(7), vText("live")), false},
		{"table gone from the schema", ident{table: "nope", basis: BasisFit, kind: kindRowid}, rowidRow(1, vInt(7), vText("live")), false},
		{"without rowid equal", ident{table: "w", basis: BasisFit, kind: kindWithoutR}, &RecoveredRow{Values: []Value{vText("k"), vText("v")}}, true},
		{"without rowid another value", ident{table: "w", basis: BasisFit, kind: kindWithoutR}, &RecoveredRow{Values: []Value{vText("k"), vText("x")}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fitLivePass(t).equalsLiveFit(c.id, c.row)
			if err != nil || got != c.want {
				t.Errorf("equalsLiveFit = %v, %v; want %v, nil", got, err, c.want)
			}
		})
	}
}

// TestEqualsLiveRowUnreadValueIsInconclusive: an unread integer, real or NULL
// and an omitted or clipped text is never taken for the live value (the rule
// of compareValues): the row is not annotated.
func TestEqualsLiveRowUnreadValueIsInconclusive(t *testing.T) {
	id := ident{table: "t", basis: BasisFit, kind: kindRowid}
	for _, c := range []struct {
		name string
		vals []Value
	}{
		{"unread int", []Value{{Kind: KindInt, Omitted: true, Unread: true, Serial: 1}, vText("live")}},
		{"unread int that would equal the live value by luck", []Value{{Kind: KindInt, Int: 7, Omitted: true, Unread: true, Serial: 1}, vText("live")}},
		{"omitted text", []Value{vInt(7), vOmitted(vText("live"))}},
		{"clipped text", []Value{vInt(7), vClipped(vText("live"), 2)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fitLivePass(t).equalsLiveFit(id, rowidRow(1, c.vals...))
			if err != nil || got {
				t.Errorf("equalsLiveFit = %v, %v: an inconclusive comparison must not annotate", got, err)
			}
		})
	}
}

// fitBudgetPass is a rowPass over a database with two WITHOUT ROWID tables of
// three rows each, w1 and w2, and the given limits.
func fitBudgetPass(t *testing.T, lim Limits) *rowPass {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	for _, n := range []string{"w1", "w2"} {
		w := b.CreateTableWithoutRowid(n, "create table "+n+"(k primary key, v) without rowid", 1)
		for i, k := range []string{"a", "b", "c"} {
			w.Insert(int64(i), k, "v")
		}
	}
	db := b.Bytes()
	d, err := Open(bytes.NewReader(db), int64(len(db)), Options{Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	l := h.d.env.newLedger()
	t.Cleanup(func() { l.free(l.n) })
	return &rowPass{h: h, ctx: context.Background(), l: l, tables: map[string]*Table{}, wr: map[string]*wrSet{}, limits: map[string]bool{}}
}

// TestFitEqualityNeverSpendsTheSchemaBudget (J80): the fit-only equality (C45)
// has a budget of its own. With MaxDiffRowsTotal exactly the size of one live
// table, a fit-named table visited first must leave the schema-named results
// of the next table what they are without C45.
func TestFitEqualityNeverSpendsTheSchemaBudget(t *testing.T) {
	lim := Limits{MaxDiffRowsTotal: 3}
	row := &RecoveredRow{Values: []Value{vText("a"), vText("v")}}
	schemaID := ident{table: "w2", basis: BasisSchema, kind: kindWithoutR}
	base, err := fitBudgetPass(t, lim).compare(schemaID, row)
	if err != nil || base.kind != cmpSame {
		t.Fatalf("baseline without C45: %+v, %v; want cmpSame", base, err)
	}
	rp := fitBudgetPass(t, lim)
	if got, err := rp.equalsLiveFit(ident{table: "w1", basis: BasisFit, kind: kindWithoutR}, row); err != nil || !got {
		t.Fatalf("fit-named w1: %v, %v; want an annotation", got, err)
	}
	got, err := rp.compare(schemaID, row)
	if err != nil || got != base {
		t.Errorf("schema-named result after a fit-named table: %+v, %v; the baseline is %+v", got, err, base)
	}
	for _, name := range rp.st.LimitsHit {
		if name == "MaxDiffRowsTotal" {
			t.Errorf("the fit-only equality hit the schema budget: %v", rp.st.LimitsHit)
		}
	}
}

// TestFitEqualityBudgetIsBounded (J80): the budget of its own is a bound, not
// an exemption. When it is spent the row is not annotated and the cut is a
// counted limit, never silent.
func TestFitEqualityBudgetIsBounded(t *testing.T) {
	rp := fitBudgetPass(t, Limits{})
	rp.fitDiffMax = 2 // below the three live rows of w1
	row := &RecoveredRow{Values: []Value{vText("a"), vText("v")}}
	got, err := rp.equalsLiveFit(ident{table: "w1", basis: BasisFit, kind: kindWithoutR}, row)
	if err != nil || got {
		t.Errorf("equalsLiveFit = %v, %v with the budget spent; want false, nil", got, err)
	}
	if len(rp.st.LimitsHit) != 1 || rp.st.LimitsHit[0] != "FitEqualityBudget" {
		t.Errorf("LimitsHit %v, want [FitEqualityBudget]", rp.st.LimitsHit)
	}
}
