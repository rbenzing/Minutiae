package sqlitedb_test

// Column DEFAULT values convert only when exact (Task 1b, ruling U3): an
// undecided default has its own state and a typed reason, so the examiner sees
// that the value is not known and why.

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestUndecidedDefaultHasItsOwnState(t *testing.T) {
	cases := []struct {
		name, decl string
		reason     sqlitefile.DefaultReason
	}{
		{"20-digit text on INTEGER", "integer default '12345678901234567890'", sqlitefile.DefaultReasonInexactLiteral},
		{"17 significant digits on NUMERIC", "numeric default '0.12345678901234567'", sqlitefile.DefaultReasonInexactLiteral},
		{"1e23 on REAL", "real default '1e23'", sqlitefile.DefaultReasonInexactLiteral},
		{"1e400 on INTEGER", "integer default '1e400'", sqlitefile.DefaultReasonInexactLiteral},
		{"a REAL needed as TEXT", "text default 0.25", sqlitefile.DefaultReasonInexactLiteral},
		{"a DDL literal that is not a float64", "default 0.1234567890123456789", sqlitefile.DefaultReasonInexactLiteral},
		{"a DDL literal of 20 digits", "default 12345678901234567890", sqlitefile.DefaultReasonInexactLiteral},
		{"an expression", "default (1+1)", sqlitefile.DefaultReasonExpression},
		{"current_timestamp", "default current_timestamp", sqlitefile.DefaultReasonExpression},
	}
	for _, k := range cases {
		for _, enc := range []int{1, 2, 3} {
			tb := tableOf(t, sqlitetest.Options{Encoding: enc}, "create table t(a, c "+k.decl+")", shortRecord)
			scanEach(t, tb, func(_ int, r sqlitedb.Row) {
				if r.State(1) != sqlitedb.StateDefaultUndecided {
					t.Fatalf("%s (enc %d): state %v, want default-undecided", k.name, enc, r.State(1))
				}
				if r.State(1).Known() || !r.Unknown(1) || r.IsNull(1) {
					t.Errorf("%s: Known %v Unknown %v IsNull %v", k.name, r.State(1).Known(), r.Unknown(1), r.IsNull(1))
				}
				if r.State(1).String() != "default-undecided" {
					t.Errorf("%s: state name %q", k.name, r.State(1).String())
				}
				if got := r.UndecidedDefault(1); got != k.reason {
					t.Errorf("%s: reason %v, want %v", k.name, got, k.reason)
				}
				if got := r.UndecidedDefault(0); got != sqlitefile.DefaultReasonNone {
					t.Errorf("%s: the stored column has reason %v", k.name, got)
				}
				if _, ok := r.Int(1); ok {
					t.Errorf("%s: Int answered for an undecided default", k.name)
				}
				if _, ok := r.Text(1); ok {
					t.Errorf("%s: Text answered for an undecided default", k.name)
				}
				if _, ok := r.Float(1); ok {
					t.Errorf("%s: Float answered for an undecided default", k.name)
				}
				if _, _, err := sqlitedb.KeyOf(r, 1); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
					t.Errorf("%s: KeyOf error %v, want ErrKeyUndecidable", k.name, err)
				}
				if r.Flags()&sqlitedb.FlagUnknownValues == 0 || r.Flags()&sqlitedb.FlagShortRecord == 0 {
					t.Errorf("%s: flags %b lack unknown-values or short-record", k.name, r.Flags())
				}
			})
		}
	}
}

// TestInexactAndExpressionDefaultsAreToldApart (U21): the two causes carry
// different reasons with different texts.
func TestInexactAndExpressionDefaultsAreToldApart(t *testing.T) {
	reasons := map[sqlitefile.DefaultReason]bool{}
	for _, decl := range []string{"integer default '12345678901234567890'", "default (1+1)"} {
		tb := tableOf(t, sqlitetest.Options{}, "create table t(a, c "+decl+")", shortRecord)
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			reasons[r.UndecidedDefault(1)] = true
			if r.UndecidedDefault(1).String() == "" {
				t.Errorf("%s: the reason has no text", decl)
			}
		})
	}
	if len(reasons) != 2 {
		t.Errorf("the two causes were not told apart: %v", reasons)
	}
}

// TestExactDefaultStaysKnownDefaulted (PASSES ON ARRIVAL: exact defaults already
// are StateDefaulted; mutation in the report: mark every literal default
// undecided): an exact default is a Known, Defaulted value.
func TestExactDefaultStaysKnownDefaulted(t *testing.T) {
	const decl = "create table t(a, b integer default '8', c real default 0.5, d text default 'x', e numeric default '1e3', f default 7, g integer default 9.2e18)"
	tb := tableOf(t, sqlitetest.Options{}, decl, shortRecord)
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		for col := 1; col <= 6; col++ {
			if r.State(col) != sqlitedb.StateDefaulted || !r.State(col).Known() || r.Unknown(col) || r.UndecidedDefault(col) != sqlitefile.DefaultReasonNone {
				t.Errorf("col %d: state %v reason %v", col, r.State(col), r.UndecidedDefault(col))
			}
		}
		if v, ok := r.Int(5); !ok || v != 7 {
			t.Errorf("f = %d, %v", v, ok)
		}
		if v, ok := r.Int(6); !ok || v != 9200000000000000000 {
			t.Errorf("g = %d, %v; the engine stores the integral real 9.2e18 as an integer", v, ok)
		}
		if r.Flags()&sqlitedb.FlagUnknownValues != 0 {
			t.Errorf("flags %b: unknown-values for exact defaults", r.Flags())
		}
	})
}

// TestRecoveredShortRowWithAnUndecidedDefault: a recovered short row takes the
// same typed state, and a truncated record stays Unread (damage wins).
func TestRecoveredShortRowWithAnUndecidedDefault(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a, b integer default '12345678901234567890', c default (1+1), d)", func(*sqlitetest.Table) {})
	one := []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}}
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: one})
	if err != nil {
		t.Fatal(err)
	}
	if r.State(1) != sqlitedb.StateDefaultUndecided || r.UndecidedDefault(1) != sqlitefile.DefaultReasonInexactLiteral {
		t.Errorf("b: state %v reason %v", r.State(1), r.UndecidedDefault(1))
	}
	if r.State(2) != sqlitedb.StateDefaultUndecided || r.UndecidedDefault(2) != sqlitefile.DefaultReasonExpression {
		t.Errorf("c: state %v reason %v", r.State(2), r.UndecidedDefault(2))
	}
	if r.State(3) != sqlitedb.StateDefaulted || !r.IsNull(3) {
		t.Errorf("d: state %v", r.State(3))
	}
	rt, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: one, Truncated: true})
	if err != nil {
		t.Fatal(err)
	}
	if rt.State(1) != sqlitedb.StateUnread {
		t.Errorf("truncated record: b is %v, want unread", rt.State(1))
	}
}
