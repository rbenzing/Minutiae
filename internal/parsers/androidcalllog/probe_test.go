package androidcalllog_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestMetaIsValidClaimsTheMappedTableAndProbes(t *testing.T) {
	p := androidcalllog.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || m.Name != androidcalllog.Name || m.Emits[0].Type != "call" {
		t.Fatalf("meta %+v: %v", m, err)
	}
	// a claim is a promise of typed recovery (plan 4G): the one table the mapping maps, on the database
	if want := []parse.TableClaim{{Role: "db", Table: "calls"}}; !slices.Equal(m.Claims, want) {
		t.Errorf("claims %v, want %v", m.Claims, want)
	}
	if len(m.Inputs[0].Globs) != 2 {
		t.Errorf("primary globs %v: want calllog.db and contacts2.db", m.Inputs[0].Globs)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}
	// a contacts2.db of a release that moved the call log out has no calls table: a benign skip
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)")
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "c2", Files: map[string][]byte{"db": bld.Bytes()}}.Bundle(h, remoteContacts)
	if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.NotApplicable {
		t.Errorf("contacts2.db without calls: %+v, %v; want not-applicable", got, err)
	}
	// a calls table without the columns the mapping needs is unsupported, loudly
	bld = sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("calls", "create table calls(_id integer primary key, number text)")
	b = parsertest.Fixture{Name: "bad", Files: map[string][]byte{"db": bld.Bytes()}}.Bundle(h, remoteCalllog)
	if got, _ := p.Probe(t.Context(), b.Input(h, p, true)); got.Status != parse.UnsupportedSchema {
		t.Errorf("calls without date/duration/type: %+v", got)
	}
}
