package androidwhatsapp_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only; it opens COPIES in the test's own temporary directory)

	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func parseFixture(t *testing.T, fx parsertest.Fixture, remote string) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	b := fx.Bundle(h, remote)
	return h.RunLenient(p, b.Input(h, p, false))
}

func runVariant(t *testing.T, variant string) (parsertest.Result, parsertest.Fixture) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	return parseFixture(t, fx, remoteFor(variant)), fx
}

func warningsContaining(res parsertest.Result, s string) []parsertest.Warning {
	var out []parsertest.Warning
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, s) {
			out = append(out, w)
		}
	}
	return out
}

func wantNotes(t *testing.T, v string, res parsertest.Result, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got, ok := res.Notes[k]; !ok || got != w {
			t.Errorf("%s: note %s = %q (present %v), want %q", v, k, got, ok, w)
		}
	}
}

// Spec 6.2, R1: the generation comes from the schema of the live view, never from a query that falls back.
func TestWhatsAppGenerationDetection(t *testing.T) {
	cases := []struct {
		variant string
		notes   map[string]string
		absent  []string
	}{
		{"android-legacy-2.11", map[string]string{"generation": "legacy", "sentinel_rows": "1", "rows.messages": "21", "app": "whatsapp", "database_user": "0"}, []string{"rows.message", "message_view_source"}},
		{"android-transition-legacydata", map[string]string{"generation": "transition", "rows.messages": "16", "rows.message": "0", "message_view_source": "messages", "sentinel_rows": "1", "jid_map": "absent"}, nil},
		{"android-transition-moderndata", map[string]string{"generation": "transition", "rows.messages": "0", "rows.message": "16", "message_view_source": "messages", "sentinel_rows": "1"}, nil},
		{"android-transition-empty", map[string]string{"generation": "transition", "rows.messages": "0", "rows.message": "0", "sentinel_rows": "1"}, nil},
		{"android-modern-2021", map[string]string{"generation": "modern", "rows.message": "18", "jid_map": "absent", "app": "whatsapp"}, []string{"rows.messages", "sentinel_rows", "message_view_source"}},
		{"android-modern-2026", map[string]string{"generation": "modern", "app": "whatsapp", "database_user": "10", "rows.message": "18"}, []string{"jid_map"}},
		{"android-modern-business", map[string]string{"generation": "modern", "app": "whatsapp_business", "database_user": "0"}, nil},
	}
	for _, c := range cases {
		res, _ := runVariant(t, c.variant)
		if res.Err != nil {
			t.Errorf("%s: Parse: %v", c.variant, res.Err)
		}
		wantNotes(t, c.variant, res, c.notes)
		for _, k := range c.absent {
			if v, ok := res.Notes[k]; ok {
				t.Errorf("%s: note %s = %q, want none", c.variant, k, v)
			}
		}
		if len(warningsContaining(res, "whatsapp-both-generations")) != 0 {
			t.Errorf("%s: a both-generations warning", c.variant)
		}
	}
	// the unmapped tables the spec lists are named, in schema order
	res, _ := runVariant(t, "android-modern-2026")
	if got := res.Notes["unmapped_tables"]; !strings.Contains(got, "missed_call_logs") || !strings.Contains(got, "props") {
		t.Errorf("unmapped_tables = %q", got)
	}
}

// R1: both populated is one counted warning with both counts, the job stays complete.
func TestWhatsAppBothGenerationsWarningCountsRows(t *testing.T) {
	res, _ := runVariant(t, "android-transition-both")
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	ws := warningsContaining(res, "whatsapp-both-generations")
	if len(ws) != 1 || ws[0].Locator != "sqlite:schema" || ws[0].Reason != "whatsapp-both-generations: messages=16 message=16" {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	wantNotes(t, "both", res, map[string]string{"generation": "transition", "rows.messages": "16", "rows.message": "16"})
}

// Spec 6.2: a table created or first filled in a committed WAL frame counts; an uncommitted frame never does.
func TestWhatsAppGenerationCountsWALRows(t *testing.T) {
	res, fx := runVariant(t, "android-transition-walonly")
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	wantNotes(t, "walonly", res, map[string]string{"rows.message": "1", "rows.messages": "16"})
	ws := warningsContaining(res, "whatsapp-both-generations")
	if len(ws) != 1 || ws[0].Reason != "whatsapp-both-generations: messages=16 message=1" {
		t.Errorf("warnings %+v", res.Warnings)
	}
	delete(fx.Files, "wal")
	res = parseFixture(t, fx, remoteLive)
	if res.Err != nil {
		t.Fatalf("Parse without the WAL: %v", res.Err)
	}
	wantNotes(t, "walonly without wal", res, map[string]string{"rows.message": "0"})
	if len(warningsContaining(res, "whatsapp-both-generations")) != 0 {
		t.Errorf("a both-generations warning without the WAL: %+v", res.Warnings)
	}
}

func TestWhatsAppNeitherTableFailsLoudlyAtParse(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "android-neither")
	fx.Files["wal"] = []byte{}
	res := parseFixture(t, fx, remoteLive)
	if !errors.Is(res.Err, androidwhatsapp.ErrNeitherTable) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records; want ErrNeitherTable and none", res.Err, len(res.Records))
	}
}

// zeroRoot returns db with the root page of table cleared (the engine says where it is).
func zeroRoot(t *testing.T, db []byte, table string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	var root, ps int
	if err := e.QueryRow("select rootpage from sqlite_master where type='table' and name=?", table).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := e.QueryRow("pragma page_size").Scan(&ps); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), db...)
	clear(out[(root-1)*ps : root*ps])
	return out
}

// Spec 6.2 (forced by R1): an unreadable table is stated, never reported as empty.
func TestWhatsAppUnreadableTableIsStatedNotEmpty(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "android-modern-business")
	fx.Files["db"] = zeroRoot(t, fx.Files["db"], "message")
	res := parseFixture(t, fx, remoteBusiness)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	wantNotes(t, "unreadable", res, map[string]string{"message": "unreadable", "generation": "modern"})
	if v, ok := res.Notes["rows.message"]; ok {
		t.Errorf("rows.message = %q for an unreadable table: the rows are unknown, not counted", v)
	}
	// the decoder's page warning says the same (the mapping scans the table)
	page := false
	for _, w := range res.Warnings {
		page = page || strings.HasPrefix(w.Locator, "sqlite:file=")
	}
	if !page {
		t.Errorf("no decoder page warning for the unreadable table: %+v", res.Warnings)
	}
	if len(res.Records) != 0 {
		t.Errorf("%d records from an unreadable table", len(res.Records))
	}
}

// Spec 6.2: a view named like a message table is not a table.
func TestWhatsAppViewNamedLikeTableIsNoted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.db")
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"create table message (_id integer primary key, chat_row_id integer, from_me integer, key_id text, timestamp integer, message_type integer)",
		"insert into message (chat_row_id, from_me, key_id, timestamp, message_type) values (1, 0, 'K', 1600000000000, 0)",
		"create view messages as select * from message",
	} {
		if _, err := e.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	res := parseFixture(t, parsertest.Fixture{Name: "view", Files: map[string][]byte{"db": db}}, remoteLive)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	wantNotes(t, "view", res, map[string]string{"generation": "modern", "views_named_like_tables": "messages", "rows.message": "1"})
	if v, ok := res.Notes["rows.messages"]; ok {
		t.Errorf("rows.messages = %q for a view", v)
	}
}

// Spec 6.2: a view that disagrees with where the rows are is one warning, never a reason to skip a table.
func TestWhatsAppMessageViewDisagreesIsWarned(t *testing.T) {
	res, _ := runVariant(t, "android-transition-moderndata")
	ws := warningsContaining(res, "whatsapp-message-view-disagrees")
	if len(ws) != 1 || ws[0].Locator != "sqlite:schema" || ws[0].Reason != "whatsapp-message-view-disagrees: message_view reads messages but rows are in message" {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	for _, v := range []string{"android-transition-legacydata", "android-transition-both", "android-transition-empty"} {
		r, _ := runVariant(t, v)
		if len(warningsContaining(r, "whatsapp-message-view-disagrees")) != 0 {
			t.Errorf("%s: a disagreement warning: %+v", v, r.Warnings)
		}
	}
}

// 4I-R4 (h): the view SQL is recorded for the examiner, never used.
func TestWhatsAppViewSQLIsRecordedByHash(t *testing.T) {
	res, _ := runVariant(t, "android-transition-legacydata")
	for _, k := range []string{"view_sql.available_message_view", "view_sql.legacy_available_messages_view"} {
		if v := res.Notes[k]; !strings.HasPrefix(v, "sha256=") || !strings.Contains(v, ";bytes=") {
			t.Errorf("note %s = %q", k, v)
		}
	}
}

func TestWhatsAppNotesStateTheRowsOfDeletedChatJobs(t *testing.T) {
	res, _ := runVariant(t, "android-modern-2026")
	wantNotes(t, "modern-2026", res, map[string]string{"deleted_chat_jobs": "0"})
}
