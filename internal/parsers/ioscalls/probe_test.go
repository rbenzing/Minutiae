package ioscalls_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
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
	bundle := parsertest.Fixture{Name: "probe", Files: map[string][]byte{"db": b.Bytes()}}.BundleIOS(h, remoteCore, logicalCore)
	p := ioscalls.Parser{}
	got, err := p.Probe(t.Context(), bundle.Input(h, p, true))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 4E-R30: each table set is applicable on its own, neither is not-applicable, and a present table that
// lacks a column the mapping needs is unsupported-schema naming it.
func TestMetaAndProbe(t *testing.T) {
	p := ioscalls.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || len(m.Claims) != 2 || m.Name != "ios.calls" ||
		m.Version != ioscalls.Version || m.Emits[0].Type != "call" {
		t.Fatalf("meta %+v: %v", m, err)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS {
		t.Errorf("platforms %v", m.Platforms)
	}
	want := []string{"ios:*/Library/CallHistoryDB/CallHistory.storedata", "ios:*/Library/CallHistory/call_history.db"}
	if g := m.Inputs[0].Globs; len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
		t.Errorf("primary globs %v", g)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		remote, logical := remotesFor(v)
		b := parsertest.LoadFixture(t, "testdata", v).BundleIOS(h, remote, logical)
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}
	const coreOK = "create table ZCALLRECORD(Z_PK integer primary key, ZDATE timestamp)"
	const legacyOK = "create table call(ROWID integer primary key autoincrement, date integer)"
	if got := probeOf(t, map[string]string{"ZCALLRECORD": coreOK}); got.Status != parse.Applicable {
		t.Errorf("ZCALLRECORD alone: %+v", got)
	}
	if got := probeOf(t, map[string]string{"call": legacyOK}); got.Status != parse.Applicable {
		t.Errorf("call alone: %+v", got)
	}
	got := probeOf(t, map[string]string{"other": "create table other(id integer primary key, x text)"})
	if got.Status != parse.NotApplicable || !strings.Contains(got.Reason, "ZCALLRECORD") || !strings.Contains(got.Reason, "call") {
		t.Errorf("neither table: %+v, want not-applicable naming both", got)
	}
	got = probeOf(t, map[string]string{"ZCALLRECORD": "create table ZCALLRECORD(Z_PK integer primary key, ZANSWERED integer)"})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZDATE") {
		t.Errorf("ZCALLRECORD without ZDATE: %+v, want unsupported naming ZDATE", got)
	}
	got = probeOf(t, map[string]string{"call": "create table call(ROWID integer primary key autoincrement, address text)"})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "date") {
		t.Errorf("call without date: %+v, want unsupported naming date", got)
	}
	// a present table that lacks columns is not rescued by the other table being complete
	got = probeOf(t, map[string]string{
		"ZCALLRECORD": coreOK,
		"call":        "create table call(ROWID integer primary key autoincrement, address text)",
	})
	if got.Status != parse.UnsupportedSchema {
		t.Errorf("a complete ZCALLRECORD beside a broken call: %+v, want unsupported", got)
	}
	// the join by rowid needs the alias
	got = probeOf(t, map[string]string{"ZCALLRECORD": "create table ZCALLRECORD(Z_PK integer, ZDATE timestamp)"})
	if got.Status != parse.UnsupportedSchema {
		t.Errorf("Z_PK that is not the rowid alias: %+v", got)
	}
}
