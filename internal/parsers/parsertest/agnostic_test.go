package parsertest

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// mapperStub maps table t; sourceDependent makes it behave differently on a recovered row.
type mapperStub struct {
	WellBehaved
	sourceDependent bool
	errOnRecovered  bool // the mapper fails, with a row error, only on a recovered row
}

func (p mapperStub) Mappings() []parse.TableMapping {
	return []parse.TableMapping{{
		Table:   "t",
		Columns: []parse.ColumnSpec{{Name: "a", Required: true}},
		Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			a, _ := row.Int(m.Column("t", "a"))
			body := "a"
			if r, ok := row.(interface {
				Recovered() *sqlitedb.RecoveredInfo
			}); ok && r.Recovered() != nil && p.sourceDependent {
				body = "recovered"
			}
			if r, ok := row.(interface {
				Recovered() *sqlitedb.RecoveredInfo
			}); ok && r.Recovered() != nil && p.errOnRecovered {
				return nil, errors.New("recovered row refused")
			}
			id, _ := row.Rowid()
			return []records.Record{{Type: "message", Locator: "sqlite:table=t;rowid=" + string(rune('0'+id)), Body: body + string(rune('0'+a))}}, nil
		},
	}}
}

func agnosticBundle(t *testing.T) (*Harness, Bundle) { return agnosticBundleRows(t, true) }

func agnosticBundleRows(t *testing.T, rows bool) (*Harness, Bundle) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(id integer primary key, a integer)")
	if rows {
		tb.Insert(1, int64(1), int64(5))
		tb.Insert(2, int64(2), int64(7))
	}
	h := NewHarness(t)
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": b.Bytes()}}
	return h, fx.Bundle(h, "/data/parsertest/mutdata.dat")
}

func TestAssertMapperSourceAgnosticAcceptsAPureMapper(t *testing.T) {
	h, b := agnosticBundle(t)
	rec := &recorder{TB: t}
	AssertMapperSourceAgnostic(rec, h, mapperStub{WellBehaved: WellBehaved{Name: "ag", Version: "1.0.0"}}, b, 10)
	if rec.Failed() {
		t.Errorf("a pure mapper was refused: %v", rec.errs)
	}
}

func TestAssertMapperSourceAgnosticCatchesASourceDependentMapper(t *testing.T) {
	h, b := agnosticBundle(t)
	rec := &recorder{TB: t}
	AssertMapperSourceAgnostic(rec, h, mapperStub{WellBehaved: WellBehaved{Name: "ag", Version: "1.0.0"}, sourceDependent: true}, b, 10)
	if !rec.Failed() {
		t.Error("a mapper that depends on the row's source was accepted")
	}
}

// The live mapping succeeds and the rebuilt one fails: the error branch must notice it.
func TestAssertMapperSourceAgnosticCatchesALiveErrorAgainstARebuiltError(t *testing.T) {
	h, b := agnosticBundle(t)
	rec := &recorder{TB: t}
	AssertMapperSourceAgnostic(rec, h, mapperStub{WellBehaved: WellBehaved{Name: "ag", Version: "1.0.0"}, errOnRecovered: true}, b, 10)
	if !rec.Failed() || !strings.Contains(strings.Join(rec.errs, " "), "rebuilt error") {
		t.Errorf("a mapper that fails only on a recovered row was accepted: %v", rec.errs)
	}
}

// A check over a table without rows would pass for any mapper: it must say so.
func TestAssertMapperSourceAgnosticRefusesAVacuousRun(t *testing.T) {
	h, b := agnosticBundleRows(t, false)
	rec := &recorder{TB: t}
	AssertMapperSourceAgnostic(rec, h, mapperStub{WellBehaved: WellBehaved{Name: "ag", Version: "1.0.0"}}, b, 10)
	if !rec.Failed() || !strings.Contains(strings.Join(rec.errs, " "), "vacuous") {
		t.Errorf("a run that checked no row was accepted: %v", rec.errs)
	}
}
