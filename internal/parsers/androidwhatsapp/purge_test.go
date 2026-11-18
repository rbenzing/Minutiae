package androidwhatsapp_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// The scenario of plan 4I task 9 (spec 6.3 as amended by G1, 4I-R4): android-modern-2026 holds the chats of messages
// 60 to 71 and the eight deleted_chat_job rows; the facts below are written from the rules, not read back from the
// generator.

func purgeRaw(t *testing.T, r records.Record) map[string]any { t.Helper(); return rawMap(t, r) }

func hasNoPurgeState(raw map[string]any) bool {
	return raw["pending_purge"] == nil && raw["pending_purge_jobs"] == nil && raw["pending_purge_job"] == nil && raw["pending_purge_reason"] == nil
}

// 4I-R4 (c), D5: a row a job covers is Deleted by the app's own structure (never live), with no Confidence, no Recovery
// and no time of deletion; a starred row is judged by the starred bound only; a chat with no job clears nothing.
func TestWhatsAppClearedChatRowsAreDeletedNotLive(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	for _, id := range []string{"60", "61", "62"} {
		r := modern["message/"+id]
		d := deletedOf(r)
		if !r.Deleted || d["source"] != "deleted_chat_job" || fmt.Sprint(d["job"]) != "1" || r.Recovery != "" || r.Confidence != nil {
			t.Errorf("message %s: deleted %v %v recovery %q confidence %v", id, r.Deleted, d, r.Recovery, r.Confidence)
		}
		for _, nt := range r.Times {
			if nt.Kind == "deleted" {
				t.Errorf("message %s has a deleted time %v (the job stores none)", id, nt)
			}
		}
		if fmt.Sprint(purgeRaw(t, r)["pending_purge_jobs"]) != "[1]" {
			t.Errorf("message %s: pending_purge_jobs %v", id, purgeRaw(t, r)["pending_purge_jobs"])
		}
	}
	// 63 is starred, its sort_id is below job 1's bound, and the starred bound is NULL: live. 64 is above the bound.
	for _, id := range []string{"63", "64"} {
		r := modern["message/"+id]
		if r.Deleted || deletedOf(r) != nil || !hasNoPurgeState(purgeRaw(t, r)) {
			t.Errorf("message %s: deleted %v %v raw %v, want live", id, r.Deleted, deletedOf(r), purgeRaw(t, r))
		}
	}
	if r := modern["message/71"]; !r.Deleted || fmt.Sprint(deletedOf(r)["job"]) != "7" {
		t.Errorf("message 71 (starred, job 7's starred bound): deleted %v %v", r.Deleted, deletedOf(r))
	}
	// the reaction on message 60 is hidden with its parent
	rx := modern["message_add_on/9"]
	if !rx.Deleted || deletedOf(rx)["source"] != "deleted_chat_job" || purgeRaw(t, rx)["deleted_via"] != "parent" {
		t.Errorf("reaction 9: deleted %v %v raw %v", rx.Deleted, deletedOf(rx), purgeRaw(t, rx))
	}
	if r1 := modern["message_add_on/1"]; r1.Deleted || purgeRaw(t, r1)["deleted_via"] != nil {
		t.Errorf("reaction 1 (a chat with no job) is deleted: %v", purgeRaw(t, r1))
	}

	// a legacy row of a transition store: the chat is reached through jid.raw_string and chat.jid_row_id
	legacy, _ := recordsOf(t, "android-transition-legacydata")
	if r := legacy["messages/9"]; !r.Deleted || deletedOf(r)["source"] != "deleted_chat_job" || fmt.Sprint(deletedOf(r)["job"]) != "1" {
		t.Errorf("legacy message 9: deleted %v %v", r.Deleted, deletedOf(r))
	}
	for _, id := range []string{"2", "10", "11"} {
		if r := legacy["messages/"+id]; r.Deleted || !hasNoPurgeState(purgeRaw(t, r)) {
			t.Errorf("legacy message %s: deleted %v raw %v, want live", id, r.Deleted, purgeRaw(t, r))
		}
	}
	if _, has := legacy["messages/1"]; has {
		t.Errorf("the legacy sentinel row is a record")
	}

	// no record of any variant is cleared without a job of its own chat
	for _, v := range parsedVariants {
		recs, _ := recordsOf(t, v)
		for k, r := range recs {
			if deletedOf(r)["source"] != "deleted_chat_job" {
				continue
			}
			if v != "android-modern-2026" && v != "android-transition-legacydata" {
				t.Errorf("%s %s: cleared in a variant with no job", v, k)
			}
			if jobs, _ := purgeRaw(t, r)["pending_purge_jobs"].([]any); len(jobs) == 0 {
				t.Errorf("%s %s: cleared with no job listed", v, k)
			}
		}
	}
}

// Review Focus 5: a row is cleared when any job of its own chat covers it. Chat C has job 2 (below its rows) and job 3
// (at 66): both rows are cleared by job 3 alone. Job 8 names no chat: counted, never applied.
func TestWhatsAppJobsForOneChatCombineByAnyBound(t *testing.T) {
	modern, res := recordsOf(t, "android-modern-2026")
	for _, id := range []string{"65", "66"} {
		r := modern["message/"+id]
		if !r.Deleted || fmt.Sprint(deletedOf(r)["job"]) != "3" || fmt.Sprint(purgeRaw(t, r)["pending_purge_jobs"]) != "[3]" {
			t.Errorf("message %s: deleted %v %v jobs %v, want job 3 alone", id, r.Deleted, deletedOf(r), purgeRaw(t, r)["pending_purge_jobs"])
		}
	}
	wantNotes(t, "modern", res, map[string]string{"deleted_chat_jobs": "8", "deleted_chat_jobs_dangling": "1"})
	for k, r := range modern {
		raw := purgeRaw(t, r)
		if fmt.Sprint(raw["pending_purge_job"]) == "8" || strings.Contains(fmt.Sprint(raw["pending_purge_jobs"]), "8") {
			t.Errorf("%s names job 8, whose chat does not exist: %v", k, raw)
		}
	}
}

// 4I-R4 (e), 4I-R9: what the parser cannot evaluate is stated, never silent and never claimed: rows 67 and 68 (a category
// list), 69 (a singular list) and 70 (a TEXT bound) stay live with raw.pending_purge, one warning per such job.
func TestWhatsAppDeletedChatJobUndecidedIsStated(t *testing.T) {
	modern, res := recordsOf(t, "android-modern-2026")
	want := map[string]struct{ job, reason string }{
		"67": {"4", "categories-or-singular-list-not-interpreted"}, "68": {"4", "categories-or-singular-list-not-interpreted"},
		"69": {"5", "categories-or-singular-list-not-interpreted"}, "70": {"6", "job-bound-not-integer"},
	}
	for id, w := range want {
		r := modern["message/"+id]
		raw := purgeRaw(t, r)
		if r.Deleted || deletedOf(r) != nil || raw["pending_purge"] != "undecided" || fmt.Sprint(raw["pending_purge_job"]) != w.job || raw["pending_purge_reason"] != w.reason {
			t.Errorf("message %s: deleted %v raw %v, want undecided by job %s (%s)", id, r.Deleted, raw, w.job, w.reason)
		}
	}
	got := map[string]int{}
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "whatsapp-pending-purge-undecided") {
			got[w.Locator]++
			if !strings.HasPrefix(w.Locator, "sqlite:table=deleted_chat_job;rowid=") {
				t.Errorf("locator %q", w.Locator)
			}
		}
	}
	for _, j := range []string{"4", "5", "6"} {
		if got["sqlite:table=deleted_chat_job;rowid="+j] != 1 {
			t.Errorf("warnings for job %s: %v, want exactly one", j, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("warnings for jobs %v, want jobs 4, 5 and 6 only", got)
	}
	if ws := warningsContaining(res, "job 4: categories-or-singular-list-not-interpreted"); len(ws) != 1 {
		t.Errorf("the text of the warning: %v", res.Warnings)
	}
}

// 4I-R4 (f): past the cap, or on a table that cannot be read whole, the first jobs are kept, one counted warning is
// raised, every row a read job does not clear is undecided, every record is still emitted, and Parse then returns
// ErrPurgeJobsIncomplete so the job is incomplete.
func TestWhatsAppDeletedChatJobCapIsIncomplete(t *testing.T) {
	full, _ := recordsOf(t, "android-modern-2026")
	fx := parsertest.LoadFixture(t, "testdata", "android-modern-2026")
	check := func(name string, res parsertest.Result, warn string) {
		t.Helper()
		if !errors.Is(res.Err, androidwhatsapp.ErrPurgeJobsIncomplete) {
			t.Errorf("%s: Parse = %v, want ErrPurgeJobsIncomplete", name, res.Err)
		}
		if len(res.Records) != len(full) {
			t.Errorf("%s: %d records, want the uncapped run's %d", name, len(res.Records), len(full))
		}
		if ws := warningsContaining(res, warn); len(ws) != 1 || ws[0].Locator != "sqlite:table=deleted_chat_job" {
			t.Errorf("%s: warnings %v, want one %q at the table", name, res.Warnings, warn)
		}
		for _, r := range res.Records {
			k := keyOf(r)
			raw := purgeRaw(t, r)
			switch k {
			case "message/60", "message/61", "message/62", "message/65", "message/66", "message_add_on/9":
				continue // cleared by jobs 1 and 3, which were read
			}
			if raw["parent_state"] != nil {
				continue // a reaction with no parent has no chat to judge
			}
			if raw["pending_purge"] != "undecided" || raw["pending_purge_reason"] != "job-set-incomplete" {
				t.Errorf("%s: %s: raw %v, want undecided (job-set-incomplete)", name, k, raw)
			}
		}
	}

	h := parsertest.NewHarness(t)
	b := fx.Bundle(h, remoteUser10)
	capped := androidwhatsapp.CappedParser{Max: 3}
	res := h.RunLenient(capped, b.Input(h, capped, false))
	check("cap 3", res, "whatsapp-deleted-chat-job-incomplete: 8 rows, 3 read")
	for _, r := range res.Records {
		if k := keyOf(r); (k == "message/60" || k == "message/61" || k == "message/62") && (!r.Deleted || fmt.Sprint(deletedOf(r)["job"]) != "1") {
			t.Errorf("%s was cleared by job 1, which was read: %v", k, deletedOf(r))
		}
		if k := keyOf(r); k == "message/64" && fmt.Sprint(purgeRaw(t, r)["pending_purge_job"]) != "1" {
			t.Errorf("message 64: job %v named, want job 1 of its chat, which was read", purgeRaw(t, r)["pending_purge_job"])
		}
		if k := keyOf(r); (k == "message/65" || k == "message/66") && (!r.Deleted || fmt.Sprint(deletedOf(r)["job"]) != "3") {
			t.Errorf("%s was cleared by job 3, which was read: %v", k, deletedOf(r))
		}
		if k := keyOf(r); k == "message/71" && purgeRaw(t, r)["pending_purge_job"] != nil {
			t.Errorf("message 71 names job %v, though no job of its chat was read", purgeRaw(t, r)["pending_purge_job"])
		}
	}

	// a table that cannot be read whole
	fx.Files["db"] = zeroRoot(t, fx.Files["db"], "deleted_chat_job")
	res = parseFixture(t, fx, remoteUser10)
	check("zeroed root page", res, "whatsapp-deleted-chat-job-incomplete")
	if ws := warningsContaining(res, "whatsapp-deleted-chat-job-incomplete"); len(ws) == 1 && !strings.Contains(ws[0].Reason, "deleted_chat_job") {
		t.Errorf("the warning does not name the unreadable table: %q", ws[0].Reason)
	}
}

// 4I-R4 (e): a job whose chat_row_id is not an INTEGER is unusable. It is counted in a note, warned about, and makes the
// rows of every chat with no clearing job undecided; the job itself stays complete.
func TestWhatsAppUnusableJobMakesOtherRowsUndecided(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "one")
		tb["deleted_chat_job"].Insert(1, nil, "one", nil, nil, nil, nil)
	})
	res := runBytes(t, db, nil, remoteLive)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	raw := rawMap(t, byRowid(t, res)["message/1"])
	if raw["pending_purge"] != "undecided" || raw["pending_purge_reason"] != "job-set-incomplete" || raw["pending_purge_job"] != nil {
		t.Errorf("raw %v, want undecided (job-set-incomplete) with no job", raw)
	}
	wantNotes(t, "unusable", res, map[string]string{"deleted_chat_jobs_unusable": "1"})
	if ws := warningsContaining(res, "whatsapp-pending-purge-undecided: deleted_chat_job 1: chat-row-id-not-integer"); len(ws) != 1 {
		t.Errorf("warnings %v", res.Warnings)
	}
}

// engineIDs runs a query on a COPY of the database in the engine and returns the first column of every row.
func engineIDs(t *testing.T, db []byte, query string) map[int64]bool {
	t.Helper()
	p := filepath.Join(t.TempDir(), "v.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	rows, err := e.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A test-only cross-check of the legacy form: the engine runs the app's own view (quoted in the DDL snapshot). Every
// legacy row the parser marks cleared is absent from it; every row it leaves live whose chat has a job without a list is
// present. The parser itself never reads the view.
func TestClearedRowsAreAbsentFromTheAppsOwnView(t *testing.T) {
	recs, _ := recordsOf(t, "android-transition-legacydata")
	fx := parsertest.LoadFixture(t, "testdata", "android-transition-legacydata")
	inView := engineIDs(t, fx.Files["db"], "select _id from legacy_available_messages_view")
	cleared := 0
	for k, r := range recs {
		var id int64
		if _, err := fmt.Sscanf(k, "messages/%d", &id); err != nil {
			continue
		}
		raw := purgeRaw(t, r)
		switch {
		case r.Deleted && deletedOf(r)["source"] == "deleted_chat_job":
			cleared++
			if inView[id] {
				t.Errorf("legacy row %d is cleared by the parser but present in the app's view", id)
			}
		case raw["chat_jid"] == "120363000000000001@g.us" && !r.Deleted:
			if !inView[id] {
				t.Errorf("legacy row %d is live in a chat with a job but absent from the app's view", id)
			}
		}
	}
	if cleared == 0 {
		t.Errorf("no row was cleared: the cross-check proved nothing")
	}
}

// 4I-R4 (h): the SQL of the app's views is recorded for the examiner and never used: replacing it leaves every record
// as it was.
func TestWhatsAppViewSQLIsRecordedNotUsed(t *testing.T) {
	res, fx := runVariant(t, "android-transition-legacydata")
	for _, k := range []string{"view_sql.legacy_available_messages_view", "view_sql.available_message_view"} {
		if v := res.Notes[k]; !strings.HasPrefix(v, "sha256=") || !strings.Contains(v, ";bytes=") {
			t.Errorf("note %s = %q", k, v)
		}
	}
	p := filepath.Join(t.TempDir(), "v.db")
	if err := os.WriteFile(p, fx.Files["db"], 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"drop view legacy_available_messages_view",
		"create view legacy_available_messages_view as select * from messages where 0",
	} {
		if _, err := e.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fx.Files["db"] = changed
	res2 := parseFixture(t, fx, remoteLive)
	if res2.Err != nil {
		t.Fatalf("Parse of the copy: %v", res2.Err)
	}
	if res2.Notes["view_sql.legacy_available_messages_view"] == res.Notes["view_sql.legacy_available_messages_view"] {
		t.Errorf("the note did not change with the view SQL")
	}
	type rec struct {
		Deleted bool
		Payload map[string]any
	}
	key := func(r parsertest.Result) map[string]rec {
		out := map[string]rec{}
		for _, x := range r.Records {
			out[x.Locator] = rec{x.Deleted, x.Payload}
		}
		return out
	}
	// the locator carries the page of the row, which moves with the rewrite: compare by table and rowid
	norm := func(m map[string]rec) map[string]rec {
		out := map[string]rec{}
		for l, v := range m {
			out[keyOfLocator(l)] = v
		}
		return out
	}
	if a, b := norm(key(res)), norm(key(res2)); !reflect.DeepEqual(a, b) {
		t.Errorf("the records changed with the SQL of a view the parser must not read")
	}
}

func keyOfLocator(l string) string {
	s, _ := strings.CutPrefix(l, "sqlite:table=")
	table, rest, _ := strings.Cut(s, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}
