package androidcontacts_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeOf(t *testing.T, p parse.Parser, db []byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "probe", Files: map[string][]byte{"db": db}}.Bundle(h, fixtureRemote)
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMetaAndProbe(t *testing.T) {
	p := androidcontacts.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || len(m.Claims) != 0 || m.Name != androidcontacts.Name || m.Emits[0].Type != "contact" {
		t.Fatalf("meta %+v: %v", m, err)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, fixtureRemote)
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}
	if got := probeOf(t, p, []byte("a renamed text file, not a database\n")); got.Status != parse.NotApplicable {
		t.Errorf("text file: %+v", got)
	}
	calls := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	calls.CreateTable("calls", "create table calls(_id integer primary key, number text)")
	if got := probeOf(t, p, calls.Bytes()); got.Status != parse.NotApplicable {
		t.Errorf("a database with only a calls table: %+v", got)
	}
	noData := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	noData.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)")
	if got := probeOf(t, p, noData.Bytes()); got.Status != parse.UnsupportedSchema {
		t.Errorf("raw_contacts without data and mimetypes: %+v; a contact without its data rows would look empty", got)
	}
	badAlias := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	badAlias.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)")
	badAlias.CreateTable("data", "create table data(_id integer primary key, raw_contact_id integer, mimetype_id integer)")
	badAlias.CreateTable("mimetypes", "create table mimetypes(_id integer, mimetype text)")
	if got := probeOf(t, p, badAlias.Bytes()); got.Status != parse.UnsupportedSchema {
		t.Errorf("mimetypes._id that is not the rowid alias: %+v; a rowid join would be wrong", got)
	}
	// T69: the account and the aggregate time come from accounts and contacts by their rowid; when those tables
	// exist their join column must be the rowid alias and the columns read must exist.
	for _, tc := range []struct{ name, table, ddl string }{
		{"accounts without the rowid alias", "accounts", "create table accounts(_id integer, account_name text, account_type text)"},
		{"accounts without account_type", "accounts", "create table accounts(_id integer primary key, account_name text)"},
		{"contacts without the rowid alias", "contacts", "create table contacts(_id integer, contact_last_updated_timestamp integer)"},
		{"contacts without the timestamp", "contacts", "create table contacts(_id integer primary key, x text)"},
	} {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)")
		b.CreateTable("data", "create table data(_id integer primary key, raw_contact_id integer, mimetype_id integer)")
		b.CreateTable("mimetypes", "create table mimetypes(_id integer primary key, mimetype text)")
		b.CreateTable(tc.table, tc.ddl)
		if got := probeOf(t, p, b.Bytes()); got.Status != parse.UnsupportedSchema {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}
