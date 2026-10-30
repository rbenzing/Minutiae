package iosaddressbook_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeOf(t *testing.T, tables map[string]string) parse.Applicability {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	for name, ddl := range tables {
		b.CreateTable(name, ddl)
	}
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "probe", Files: map[string][]byte{"db": b.Bytes()}}.BundleIOS(h, remote, logical)
	p := iosaddressbook.Parser{}
	got, err := p.Probe(t.Context(), bundle.Input(h, p, true))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMetaAndProbe(t *testing.T) {
	p := iosaddressbook.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || len(m.Claims) != 0 || m.Name != "ios.addressbook" ||
		m.Version != iosaddressbook.Version || len(m.Emits) != 1 || m.Emits[0].Type != "contact" {
		t.Fatalf("meta %+v: %v", m, err)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS {
		t.Errorf("platforms %v", m.Platforms)
	}
	if g := m.Inputs[0].Globs; len(g) != 1 || g[0] != "ios:*/Library/AddressBook/AddressBook.sqlitedb" || !m.Inputs[0].Required {
		t.Errorf("primary %+v", m.Inputs[0])
	}
	roles := map[string]parse.InputSpec{}
	for _, in := range m.Inputs {
		roles[in.Role] = in
	}
	if len(roles) != 5 || len(roles["wal"].Companions) != 1 || roles["wal"].Companions[0] != "-wal" ||
		len(roles["journal"].Companions) != 1 || roles["journal"].Companions[0] != "-journal" ||
		len(roles["images"].Globs) != 1 || roles["images"].Globs[0] != "./AddressBookImages.sqlitedb" ||
		len(roles["images_wal"].Globs) != 1 || roles["images_wal"].Globs[0] != "./AddressBookImages.sqlitedb-wal" {
		t.Errorf("roles %+v", roles)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}
	const personOK = "create table ABPerson(ROWID integer primary key autoincrement, First text)"
	if got := probeOf(t, map[string]string{"ABPerson": personOK}); got.Status != parse.Applicable {
		t.Errorf("ABPerson alone: %+v", got)
	}
	got := probeOf(t, map[string]string{"other": "create table other(id integer primary key, x text)"})
	if got.Status != parse.NotApplicable || !strings.Contains(got.Reason, "ABPerson") {
		t.Errorf("no ABPerson: %+v", got)
	}
	got = probeOf(t, map[string]string{"ABPerson": "create table ABPerson(ROWID integer, First text)"})
	if got.Status != parse.UnsupportedSchema {
		t.Errorf("ROWID that is not the rowid alias: %+v", got)
	}
	// a present join table that lacks a column the join reads is unsupported, naming it
	got = probeOf(t, map[string]string{"ABPerson": personOK, "ABMultiValue": "create table ABMultiValue(UID integer primary key, record_id integer, property integer, value text)"})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "label") {
		t.Errorf("ABMultiValue without label: %+v", got)
	}
	got = probeOf(t, map[string]string{"ABPerson": personOK, "ABGroupMembers": "create table ABGroupMembers(UID integer primary key, group_id integer)"})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "member_type") {
		t.Errorf("ABGroupMembers without member_type: %+v", got)
	}
	// every join table is optional: an older or stripped database still parses its people
	all := map[string]string{
		"ABPerson":             personOK,
		"ABMultiValue":         "create table ABMultiValue(UID integer primary key, record_id integer, property integer, label integer, value text)",
		"ABMultiValueLabel":    "create table ABMultiValueLabel(value text)",
		"ABMultiValueEntry":    "create table ABMultiValueEntry(parent_id integer, key integer, value text)",
		"ABMultiValueEntryKey": "create table ABMultiValueEntryKey(value text)",
		"ABStore":              "create table ABStore(ROWID integer primary key autoincrement, Name text)",
		"ABGroup":              "create table ABGroup(ROWID integer primary key autoincrement, Name text)",
		"ABGroupMembers":       "create table ABGroupMembers(UID integer primary key, group_id integer, member_type integer, member_id integer)",
	}
	if got := probeOf(t, all); got.Status != parse.Applicable {
		t.Errorf("all tables: %+v", got)
	}
}
