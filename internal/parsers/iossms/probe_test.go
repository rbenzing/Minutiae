package iossms_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeBytes(t *testing.T, files map[string][]byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "probe", Files: files}.BundleIOS(h, dbRemote, dbLogical)
	p := iossms.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func probeTables(t *testing.T, tables map[string]string, wal bool) parse.Applicability {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	for name, ddl := range tables {
		b.CreateTable(name, ddl)
	}
	files := map[string][]byte{"db": b.Bytes()}
	if wal {
		files["wal"] = make([]byte, 32)
	}
	return probeBytes(t, files)
}

func TestMetaAndProbe(t *testing.T) {
	p := iossms.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || len(m.Claims) != 1 || m.Claims[0].Table != "message" || m.Name != "ios.sms" || m.Version != iossms.Version {
		t.Fatalf("meta %+v: %v", m, err)
	}
	if len(m.Emits) != 1 || m.Emits[0].Type != "message" || m.Emits[0].PayloadVersion != 1 {
		t.Errorf("emits %v", m.Emits)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS {
		t.Errorf("platforms %v", m.Platforms)
	}
	roles := map[string]bool{}
	for _, in := range m.Inputs {
		roles[in.Role] = true
	}
	if !roles["db"] || !roles["wal"] || !roles["journal"] || len(roles) != 3 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required {
		t.Errorf("roles %v", m.Inputs)
	}
	if g := m.Inputs[0].Globs; len(g) != 1 || g[0] != "ios:*/Library/SMS/sms.db" {
		t.Errorf("globs %v", g)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).BundleIOSBackup(h, domain, rel)
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}

	const msgOK = "create table message(ROWID integer primary key autoincrement, guid text, date integer, is_from_me integer, handle_id integer)"
	if got := probeTables(t, map[string]string{"message": msgOK}, false); got.Status != parse.Applicable {
		t.Errorf("message alone: %+v", got)
	}
	if got := probeTables(t, map[string]string{"other": "create table other(id integer primary key, x text)"}, false); got.Status != parse.NotApplicable {
		t.Errorf("no message table: %+v, want not-applicable", got)
	}
	got := probeTables(t, map[string]string{"message": "create table message(ROWID integer primary key autoincrement, guid text, date integer, handle_id integer)"}, false)
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "is_from_me") {
		t.Errorf("message without is_from_me: %+v, want unsupported naming it", got)
	}
	got = probeTables(t, map[string]string{"message": "create table message(ROWID integer, guid text, date integer, is_from_me integer, handle_id integer)"}, false)
	if got.Status != parse.UnsupportedSchema {
		t.Errorf("ROWID that is not the rowid alias: %+v", got)
	}
	got = probeTables(t, map[string]string{"message": msgOK, "chat_message_join": "create table chat_message_join(chat_id integer)"}, false)
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "message_id") {
		t.Errorf("a present join table without its column: %+v", got)
	}
	// with a -wal beside the file the main file's schema proves nothing (D4): the decision is deferred to Parse
	got = probeTables(t, map[string]string{"other": "create table other(id integer primary key, x text)"}, true)
	if got.Status != parse.Applicable || !strings.Contains(got.Reason, "parse time") {
		t.Errorf("missing message table with a -wal: %+v, want applicable with the deferred reason", got)
	}
	got = probeTables(t, map[string]string{"message": "create table message(ROWID integer primary key, guid text)"}, true)
	if got.Status != parse.Applicable || !strings.Contains(got.Reason, "parse time") {
		t.Errorf("message without columns with a -wal: %+v", got)
	}
}
