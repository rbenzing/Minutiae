package androidcontacts_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "android-telephony", "contacts-*.sql"))
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
		if !strings.HasPrefix(ddl, "contacts-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "aosp-baseline").Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "aosp-baseline").Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}

// Deleted-by-the-app is the only deleted a live parse knows (C8): contacts 3 and 11 carry the application's
// marker, and no record carries a recovery method or a confidence.
func TestOnlyTheApplicationsOwnMarkerSetsDeleted(t *testing.T) {
	res := run(t, "aosp-baseline")
	deleted := map[string]bool{}
	for _, r := range res.Records {
		if r.Recovery != "" || r.Confidence != nil {
			t.Errorf("%s: a live parse set Recovery %q, Confidence %v", r.Locator, r.Recovery, r.Confidence)
		}
		if r.Deleted {
			deleted[r.Locator] = true
		}
	}
	if len(deleted) != 2 || !deleted["sqlite:table=raw_contacts;rowid=3"] || !deleted["sqlite:table=raw_contacts;rowid=11"] {
		t.Errorf("deleted records %v, want contacts 3 and 11", deleted)
	}
	// a tombstone of deleted_contacts is also the application's own marker, never a recovery
	for _, r := range run(t, "deleted-contacts").Records {
		if r.Recovery != "" || r.Confidence != nil || (strings.Contains(r.Locator, "table=deleted_contacts") && !r.Deleted) {
			t.Errorf("%s: deleted %v recovery %q", r.Locator, r.Deleted, r.Recovery)
		}
	}
}

func TestUnicodeAndRTLNamesSurviveInBodyAndSummaryIsFreeOfBidiControls(t *testing.T) {
	res := run(t, "unicode-rtl")
	if len(res.Records) == 0 {
		t.Fatal("no records")
	}
	for _, r := range res.Records {
		name, _ := r.Payload["display_name"].(string)
		if name == "" || !strings.Contains(r.Body, name) {
			t.Errorf("%s: the searchable body lost the name %q", r.Locator, name)
		}
		if strings.ContainsAny(r.Summary, "‮‭‪‫‬⁦⁧⁨⁩") {
			t.Errorf("%s: a bidi control is in the summary: %q", r.Locator, r.Summary)
		}
	}
}

// T18, as for the call log: a contacts2.db of a release without raw_contacts, shipped with a -wal (so Probe defers
// its decision), is skipped as not applicable, audited, and is not a failed job.
func TestDatabaseWithoutRawContactsAndWithAWALIsNotApplicableNotFailed(t *testing.T) {
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("calls", "create table calls(_id integer primary key, number text, date integer)")
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "c2", Files: map[string][]byte{"db": bld.Bytes(), "wal": {}}}.Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
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
