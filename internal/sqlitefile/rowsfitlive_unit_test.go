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
