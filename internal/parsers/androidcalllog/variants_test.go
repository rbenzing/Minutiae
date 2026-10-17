package androidcalllog_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Ruling T18: a contacts2.db of a release that keeps the call log elsewhere, shipped with a -wal
// (so Probe defers its decision), is skipped as not applicable, audited, and is not a failed job.
func TestContacts2WithoutCallsAndWithAWALIsNotApplicableNotFailed(t *testing.T) {
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)")
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "c2", Files: map[string][]byte{"db": bld.Bytes(), "wal": {}}}.Bundle(h, remoteContacts)
	p := androidcalllog.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.Applicable {
		t.Fatalf("Probe = %+v, %v; with a WAL the decision is deferred to Parse", got, err)
	}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil || len(res.Records) != 0 || res.Notes["not_applicable"] == "" {
		t.Fatalf("err %v, %d records, notes %v", res.Err, len(res.Records), res.Notes)
	}
	var warned bool
	for _, w := range res.Warnings {
		warned = warned || strings.Contains(w.Reason, "not applicable at parse time")
	}
	if !warned {
		t.Errorf("no audited warning: %+v", res.Warnings)
	}
}

func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "android-telephony", "calllog-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		used[parsertest.LoadFixture(t, "testdata", v).Expect.DDL] = true
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "calllog-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "features-subid").Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "features-subid").Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}

// The same contacts2.db is also a contacts database: the call log parser must read only its table.
func TestCallLogInContacts2ReadsOnlyTheCallsTable(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "in-contacts2")
	b := fx.Bundle(h, remoteContacts)
	p := androidcalllog.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if len(res.Records) == 0 {
		t.Fatal("no records")
	}
	for _, r := range res.Records {
		if !strings.HasPrefix(r.Locator, "sqlite:table=calls;") || r.Type != "call" {
			t.Errorf("record %s of type %s", r.Locator, r.Type)
		}
	}
	if res.Notes["schema.calls"] == "" {
		t.Errorf("notes %v", res.Notes)
	}
}
