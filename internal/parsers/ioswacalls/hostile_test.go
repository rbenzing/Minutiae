package ioswacalls_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The hostile rows of spec 10 that earlier tests do not cover. Each test builds its database here, runs the parser through the
// harness and requires: no panic, no invented value, and a stated warning or raw marker. A bound is a typed error or a counted
// warning, never a silent cut.

// callBuilder is buildCalls with the builder handed back, for a test that needs a snapshot or a journal.
func callBuilder(pageSize int, fill func(tb callTables)) (*sqlitetest.Builder, callTables) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: pageSize})
	tb := callTables{}
	for _, name := range callOrder {
		d := callDDL[name]
		tb[name] = callTable{t: b.CreateTable(name, d.sql), cols: d.cols}
	}
	fill(tb)
	return b, tb
}

// hostileRun parses db (with wal and journal when not nil) at the consumer backup domain with the given MemBudget (0: the
// default). It does not fail on an error: the tests judge it.
func hostileRun(t *testing.T, db, wal, journal []byte, budget int64) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	if journal != nil {
		files["journal"] = journal
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "hostile", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswacalls.Parser{}
	in := b.Input(h, p, false)
	if budget > 0 {
		in.Budget = parse.NewBudget(budget).View()
	}
	return h.RunLenient(p, in)
}

// typedOutcome fails the test on an error that is not one of the parser's or the decoder's typed answers to a hostile file.
func typedOutcome(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil || parsertest.HostileOK(err) {
		return
	}
	for _, typed := range []error{ioswacalls.ErrNoWhatsAppTables, ioswacalls.ErrAggregatesIncomplete} {
		if errors.Is(err, typed) {
			return
		}
	}
	t.Fatalf("%s: untyped error %v", label, err)
}

// A database cut inside the call table: the pages that are gone are stated by the decoder as warnings, and nothing is guessed
// about the rows that were lost. Every cut length holds the contract (typed errors only, no refused record, no record the
// database did not hold), a cut that loses rows is never an empty complete result, and a warning of the reader is raised once
// (the parser reads the table twice: neither read may state a page twice).
func TestIOSWhatsAppCallsTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 600
	full, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		for i := int64(1); i <= rows; i++ {
			tb["ZWACDCALLEVENT"].put(i, callRow(map[string]any{"Z1CALLEVENTS": i, "ZCALLIDSTRING": fmt.Sprintf("c%d %s", i, strings.Repeat("x", 60))}))
		}
	})
	const ps = 4096
	pages := len(full) / ps
	var stated int
	for cut := pages - 1; cut >= pages-8 && cut >= 3; cut-- {
		res := hostileRun(t, slices.Clone(full[:cut*ps]), nil, nil, 0)
		typedOutcome(t, fmt.Sprintf("cut at page %d", cut), res.Err)
		if res.Refused != 0 || len(res.Violations) != 0 {
			t.Fatalf("cut at page %d: refused %d, violations %v", cut, res.Refused, res.Violations)
		}
		for _, r := range res.Records {
			if id := rawOf(r)["ZCALLIDSTRING"]; id == nil || !strings.HasPrefix(id.(string), fmt.Sprintf("c%d ", rowidOf(r.Locator))) {
				t.Fatalf("cut at page %d: the record %v was not in the database", cut, r.Locator)
			}
		}
		seen := map[string]bool{}
		for _, w := range res.Warnings {
			if k := w.Locator + "|" + w.Reason; seen[k] && strings.HasPrefix(w.Locator, "sqlite:file=") {
				t.Errorf("cut at page %d: the reader's warning %s is raised twice", cut, k)
			} else {
				seen[k] = true
			}
		}
		if len(res.Records) == rows {
			continue
		}
		if res.Err == nil && len(res.Warnings) == 0 {
			t.Errorf("cut at page %d: %d of %d rows, no error and no warning: an empty or short complete result", cut, len(res.Records), rows)
			continue
		}
		stated++
	}
	if stated == 0 {
		t.Error("no cut lost a row: the check proves nothing")
	}
}

// A damaged page of the call table: the aggregate counts cannot be trusted (an unreadable row may name any aggregate), so no
// call states a direction it could not have decided: every direction is undecided or unknown, never "in" or "out", and the job
// states the incomplete set. The damage is warned about (once) and the other rows are kept.
func TestIOSWhatsAppCallsDamagedCallPageDecidesNoDirection(t *testing.T) {
	const rows = 300
	b, _ := callBuilder(4096, func(tb callTables) {
		for i := int64(1); i <= rows; i++ {
			tb["ZWAAGGREGATECALLEVENT"].put(i, aggRow(int64(1), nil))
			tb["ZWACDCALLEVENT"].put(i, callRow(map[string]any{"Z1CALLEVENTS": i, "ZCALLIDSTRING": fmt.Sprintf("c%d %s", i, strings.Repeat("x", 60))}))
		}
	})
	good := b.Bytes()
	if r := hostileRun(t, good, nil, nil, 0); r.Err != nil || len(r.Records) != rows || r.Records[0].Payload["direction"] != "in" {
		t.Fatalf("control: err %v, %d records", r.Err, len(r.Records))
	}
	data := slices.Clone(good) // damage the leaf that holds the last call (its page type byte)
	off := strings.LastIndex(string(data), fmt.Sprintf("c%d x", rows))
	data[off/4096*4096] = 0 // an invalid page type
	res := hostileRun(t, data, nil, nil, 0)
	typedOutcome(t, "damaged leaf", res.Err)
	if len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("violations %v refused %d", res.Violations, res.Refused)
	}
	if len(res.Records) == 0 || len(res.Records) >= rows {
		t.Fatalf("%d records of %d: the damage should lose some and keep others", len(res.Records), rows)
	}
	for _, r := range res.Records {
		if d := r.Payload["direction"]; d != "unknown" || rawOf(r)["direction_state"] != "undecided" {
			t.Fatalf("call %d: direction %v state %v after a damaged scan", rowidOf(r.Locator), d, rawOf(r)["direction_state"])
		}
	}
	if !errors.Is(res.Err, ioswacalls.ErrAggregatesIncomplete) {
		t.Errorf("err %v, want ErrAggregatesIncomplete", res.Err)
	}
	var pages, incomplete int
	seen := map[string]bool{}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Locator, "sqlite:file=db;page=") {
			pages++
			if seen[w.Locator+w.Reason] {
				t.Errorf("warning %s %s raised twice", w.Locator, w.Reason)
			}
			seen[w.Locator+w.Reason] = true
		}
		if strings.Contains(w.Reason, "whatsapp-ios-aggregate-set-incomplete") {
			incomplete++
		}
	}
	if pages == 0 || incomplete != 1 {
		t.Errorf("page warnings %d, incomplete-set warnings %d: %v", pages, incomplete, res.Warnings)
	}
}

// A hot journal is rolled back: the record reflects the rolled-back state and a journal-hot warning says so.
func TestIOSWhatsAppCallsHotJournalIsRolledBack(t *testing.T) {
	const ps = 512
	b, tb := callBuilder(ps, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1), "ZCALLIDSTRING": "original"}))
	})
	snap := b.Snapshot()
	tb["ZWACDCALLEVENT"].update(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1), "ZCALLIDSTRING": "interrupted"}))
	root := tb["ZWACDCALLEVENT"].t.Root()
	db := b.Bytes()
	j := b.NewJournal(ps, 0x5eed0003, snap.Pages())
	j.Record(root, snap.Page(root))
	res := hostileRun(t, db, nil, j.Bytes(), 0)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	var ids []any
	for _, r := range res.Records {
		ids = append(ids, rawOf(r)["ZCALLIDSTRING"])
	}
	if !reflect.DeepEqual(ids, []any{"original"}) {
		t.Errorf("call ids %q, want only the rolled-back state", ids)
	}
	var hot bool
	for _, w := range res.Warnings {
		hot = hot || strings.Contains(w.Reason, "journal-hot")
	}
	if !hot {
		t.Errorf("no journal-hot warning: %+v", res.Warnings)
	}
}

// Plan 4I2 Task 7: call 15 and aggregate A12 of ios-callhistory lie only in a committed WAL frame and call 16 only in an
// uncommitted one. The record of call 15 points into the WAL artifact (its artifact id, path and range), its direction comes
// from the aggregate in the WAL (a single-call aggregate: in), and call 16 is not a record at all.
func TestIOSWhatsAppCallsWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-callhistory"), "ios-callhistory")
	wal, ok := b.ByRole["wal"]
	if !ok || wal.ID == "" || wal.ID == b.Primary.ID || wal.Source.RemotePath == b.Primary.Source.RemotePath {
		t.Fatalf("the bundle has no distinct WAL artifact: %+v", wal)
	}
	p := ioswacalls.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	byRow := byRowid(res)
	r15, has15 := byRow[15]
	if !has15 {
		t.Fatal("call 15 (committed WAL frame) is no record")
	}
	if r15.ArtifactID != wal.ID || r15.SourcePath != wal.Source.RemotePath || r15.Range == nil || r15.Range.Offset+r15.Range.Length > wal.Size {
		t.Errorf("call 15: artifact %s path %s range %+v, want the WAL artifact %s at %s", r15.ArtifactID, r15.SourcePath, r15.Range, wal.ID, wal.Source.RemotePath)
	}
	if r15.Payload["direction"] != "in" {
		t.Errorf("call 15: direction %v, want in (aggregate A12 of the WAL)", r15.Payload["direction"])
	}
	if _, has := byRow[16]; has {
		t.Error("call 16 lies only in an uncommitted frame and is a record")
	}
}

// ZDATE and ZDURATION as hostile values: a NaN, an infinity, -1, 0, a millisecond magnitude, a TEXT and a BLOB date give no
// time and state themselves (never rescaled by magnitude, spec 7.6); a huge or non-finite duration gives no end; a date and
// duration whose end would pass the representable range give no end.
func TestIOSWhatsAppCallsHostileTimes(t *testing.T) {
	type want struct {
		status, ieee string
		unknown      string
	}
	dates := []struct {
		v any
		w want
	}{
		{math.NaN(), want{status: "invalid"}},
		{math.Inf(1), want{status: "invalid", ieee: "7ff0000000000000"}},
		{math.Inf(-1), want{status: "invalid", ieee: "fff0000000000000"}},
		{int64(-1), want{status: "sentinel"}},
		{int64(0), want{status: "sentinel"}},
		{int64(math.MaxInt64), want{status: "sentinel"}},
		{int64(600000000000), want{status: "invalid"}}, // a millisecond magnitude: beyond 2100, never rescaled
		{1e300, want{status: "invalid"}},
		{"yesterday", want{unknown: "type-mismatch"}},
		{[]byte{1, 2}, want{unknown: "type-mismatch"}},
	}
	db, _ := buildCalls(nil, func(tb callTables) {
		for i, d := range dates {
			tb["ZWACDCALLEVENT"].put(int64(i+1), callRow(map[string]any{"ZDATE": d.v, "ZDURATION": float64(30)}))
		}
		n := int64(len(dates))
		tb["ZWACDCALLEVENT"].put(n+1, callRow(map[string]any{"ZDURATION": 1e300}))
		tb["ZWACDCALLEVENT"].put(n+2, callRow(map[string]any{"ZDATE": float64(600000100), "ZDURATION": float64(1e9)}))
		tb["ZWACDCALLEVENT"].put(n+3, callRow(map[string]any{"ZDATE": float64(600000100), "ZDURATION": float64(1e9 + 1)}))
		tb["ZWACDCALLEVENT"].put(n+4, callRow(map[string]any{"ZDATE": float64(3.1e9), "ZDURATION": float64(1e9)})) // an end past 2100
	})
	recs, _ := runCalls(t, db, nil)
	for i, d := range dates {
		r := recs[int64(i+1)]
		raw := rawOf(r)
		if r.Time != nil || r.TimeEnd != nil {
			t.Errorf("date %v: a time %v / end %v is invented", d.v, r.Time, r.TimeEnd)
		}
		if st, _ := raw["ZDATE_status"].(string); st != d.w.status {
			t.Errorf("date %v: status %q, want %q (raw %v)", d.v, st, d.w.status, raw)
		}
		if ie, _ := raw["ZDATE_ieee"].(string); ie != d.w.ieee {
			t.Errorf("date %v: ieee %q, want %q", d.v, ie, d.w.ieee)
		}
		uc, _ := raw["unknown_columns"].(map[string]any)
		if got, _ := uc["ZWACDCALLEVENT.ZDATE"].(string); got != d.w.unknown {
			t.Errorf("date %v: unknown_columns %v, want %q", d.v, uc, d.w.unknown)
		}
		if r.Payload["duration_s"] != 30.0 {
			t.Errorf("date %v: the duration is the call's own fact: %v", d.v, r.Payload["duration_s"])
		}
	}
	n := int64(len(dates))
	if r := recs[n+1]; r.TimeEnd != nil || r.Payload["duration_s"] != 1e300 {
		t.Errorf("duration 1e300: end %v duration %v", r.TimeEnd, r.Payload["duration_s"])
	}
	if r := recs[n+2]; r.TimeEnd == nil {
		t.Errorf("duration of 1e9 s is the largest that derives an end: %v", r.Payload)
	}
	if r := recs[n+3]; r.TimeEnd != nil || r.Payload["duration_s"] != float64(1e9+1) {
		t.Errorf("duration over 1e9 s: end %v", r.TimeEnd)
	}
	if r := recs[n+4]; r.TimeEnd != nil || r.Time == nil {
		t.Errorf("an end beyond the representable range: time %v end %v", r.Time, r.TimeEnd)
	}
}

// JIDs on the call row are raw only, as stored (cleaned, flagged): a NUL is altered text with its flag, a megabyte is cut at
// the text cap and flagged, never copied whole into raw; no participant is invented.
func TestIOSWhatsAppCallsHostileJIDs(t *testing.T) {
	huge := strings.Repeat("9", 1<<20) + "@s.whatsapp.net"
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"ZGROUPJIDSTRING": "1555\x00@g.us", "ZGROUPCALLCREATORUSERJIDSTRING": huge, "ZCALLIDSTRING": "\xff\xfe"}))
		tb["ZWACDCALLEVENT"].put(2, callRow(map[string]any{"ZGROUPJIDSTRING": int64(5), "ZGROUPCALLCREATORUSERJIDSTRING": []byte{1}, "ZCALLIDSTRING": nil}))
	})
	recs, _ := runCalls(t, db, nil)
	raw := rawOf(recs[1])
	if raw["ZGROUPJIDSTRING"] != "1555�@g.us" {
		t.Errorf("NUL: %q", raw["ZGROUPJIDSTRING"])
	}
	if s, _ := raw["ZGROUPCALLCREATORUSERJIDSTRING"].(string); len(s) == 0 || len(s) > 256<<10 {
		t.Errorf("1 MiB: %d bytes kept, want at most 256 KiB", len(s))
	}
	flags, _ := raw["text_flags"].([]any)
	for _, f := range []string{"ZCALLIDSTRING", "ZGROUPCALLCREATORUSERJIDSTRING", "ZGROUPJIDSTRING"} {
		if !slices.Contains(flags, any(f)) {
			t.Errorf("text_flags %v lacks %s", flags, f)
		}
	}
	if _, has := recs[1].Payload["address"]; has || recs[1].Payload["participants"] != nil {
		t.Errorf("an address or participant was invented: %v", recs[1].Payload)
	}
	r2 := rawOf(recs[2])
	uc, _ := r2["unknown_columns"].(map[string]any)
	if uc["ZWACDCALLEVENT.ZGROUPJIDSTRING"] != "type-mismatch" || uc["ZWACDCALLEVENT.ZGROUPCALLCREATORUSERJIDSTRING"] != "type-mismatch" || r2["ZCALLIDSTRING"] != nil {
		t.Errorf("wrong-class JID cells: %v", r2)
	}
}

// A 4 MiB text is cut at 256 KiB and flagged; a 3 MiB BLOB in a column raw states as stored is its length only; the parse stays
// inside a MemBudget of 256 MiB.
func TestIOSWhatsAppCallsHugeValuesAreCapped(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	blob := make([]byte, 3<<20)
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"ZCALLIDSTRING": big}))
		tb["ZWACDCALLEVENT"].put(2, callRow(map[string]any{"ZOUTCOME": blob, "ZVIDEO": blob, "Z1CALLEVENTS": blob}))
	})
	res := hostileRun(t, db, nil, nil, 256<<20)
	if res.Err != nil {
		t.Fatalf("Parse at MemBudget 256 MiB: %v", res.Err)
	}
	recs := byRowid(res)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2", len(recs))
	}
	r1 := rawOf(recs[1])
	if s, _ := r1["ZCALLIDSTRING"].(string); len(s) > 256<<10 || len(s) < 200<<10 || !slices.Contains(r1["text_flags"].([]any), any("ZCALLIDSTRING")) {
		t.Errorf("call 1: ZCALLIDSTRING %d bytes, text_flags %v, want the 4 MiB text cut at 256 KiB and flagged", len(s), r1["text_flags"])
	}
	r2 := rawOf(recs[2])
	for _, k := range []string{"ZOUTCOME", "ZVIDEO", "Z1CALLEVENTS"} {
		if !reflect.DeepEqual(r2[k], map[string]any{"blob_len": int64(3 << 20)}) {
			t.Errorf("call 2: raw.%s = %.80v, want {blob_len 3145728} and no base64", k, r2[k])
		}
	}
	if r2["direction_state"] != "undecided" || recs[2].Payload["outcome"] != "unknown" {
		t.Errorf("call 2: %v", recs[2].Payload)
	}
}

// A code this version has never seen is unknown with its value raw: never a label, never an outcome or kind.
func TestIOSWhatsAppCallsUnknownFutureCodes(t *testing.T) {
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(42), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"ZOUTCOME": int64(4242), "ZVIDEO": int64(77), "Z1CALLEVENTS": int64(1)}))
	})
	recs, _ := runCalls(t, db, nil)
	r, raw := recs[1], rawOf(recs[1])
	if r.Payload["outcome"] != "unknown" || r.Payload["kind"] != "unknown" || r.Payload["direction"] != "unknown" || raw["ZOUTCOME"] != int64(4242) || raw["ZVIDEO"] != int64(77) ||
		raw["label_ZOUTCOME"] != nil || raw["direction_state"] != "incoming-unknown" {
		t.Errorf("future codes: %v", r.Payload)
	}
}

// The notes of the identity (container, app) and the aggregate count are written before any mapping, so they survive the host's
// cap on notes, where the first write of a key wins and the later notes are the ones dropped. The cap is the real 64 and then
// lowered so that it bites on this fixture.
func TestIOSWhatsAppCallsNotesSurviveTheNoteCap(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-callhistory")
	run := func(limit int) parsertest.Result {
		h := parsertest.NewHarness(t)
		p := ioswacalls.Parser{}
		in := bundleOf(h, fx, "ios-callhistory").Input(h, p, false)
		in.Limits.MaxNotes = limit
		return h.RunLenient(p, in)
	}
	want := map[string]string{"container": "backup_domain", "app": "whatsapp", "app_basis": "backup_domain", "unmapped_tables": "ZWACDCALLEVENTPARTICIPANT", "aggregates": "12"}
	full := run(parse.DefaultLimits().MaxNotes)
	if full.Err != nil || len(full.Notes) > 64 || len(full.Notes) < len(want) {
		t.Fatalf("err %v, %d notes", full.Err, len(full.Notes))
	}
	for k, v := range want {
		if full.Notes[k] != v {
			t.Errorf("cap 64: note %s = %q, want %q", k, full.Notes[k], v)
		}
	}
	capped := run(len(want))
	if capped.Err != nil || len(capped.Notes) != len(want) {
		t.Fatalf("cap %d: err %v, %d notes (the cap must bite: the run writes %d)", len(want), capped.Err, len(capped.Notes), len(full.Notes))
	}
	for k, v := range want {
		if capped.Notes[k] != v {
			t.Errorf("capped: note %s = %q, want %q (written before the mapping, so kept)", k, capped.Notes[k], v)
		}
	}
}

// A schema the main file cannot vouch for (a -wal companion defers Probe's checks) is handled at Parse, never as "no row": an
// absent aggregate table is the note join_undecided.ZWAAGGREGATECALLEVENT = "table absent", every call keyed to an aggregate is
// raw.join_undecided with direction_state undecided, and one counted warning follows the reader's (PF13); an aggregate table
// that is not keyed by the rowid alias or lacks ZINCOMING is guarded with one schema warning (4I review F1).
func TestIOSWhatsAppCallsUndecidedJoinsAreStatedAndCounted(t *testing.T) {
	const counted = "whatsapp-ios-join-undecided: ZWAAGGREGATECALLEVENT: 4 records"
	// calls 1 and 2 name one aggregate each, calls 4 and 5 share aggregate 3, call 3 names none
	calls := func(tb callTables) {
		c := tb["ZWACDCALLEVENT"]
		c.put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
		c.put(2, callRow(map[string]any{"Z1CALLEVENTS": int64(2)}))
		c.put(3, callRow(nil))
		c.put(4, callRow(map[string]any{"Z1CALLEVENTS": int64(3)}))
		c.put(5, callRow(map[string]any{"Z1CALLEVENTS": int64(3)}))
	}
	for name, c := range map[string]struct {
		over     map[string]string
		agg      func(a *sqlitetest.Table)
		note     string
		schemaOK bool // one sqlite:schema warning
	}{
		"absent": {map[string]string{"ZWAAGGREGATECALLEVENT": ""}, nil, "table absent", false},
		"no alias": {
			map[string]string{"ZWAAGGREGATECALLEVENT": "CREATE TABLE ZWAAGGREGATECALLEVENT (Z_PK INTEGER, Z_ENT INTEGER, ZINCOMING INTEGER)"},
			func(a *sqlitetest.Table) {
				for i := int64(1); i <= 3; i++ {
					a.Insert(i, i, int64(2), int64(1))
				}
			}, "column Z_PK is not the rowid alias", true,
		},
		"no ZINCOMING": {
			map[string]string{"ZWAAGGREGATECALLEVENT": "CREATE TABLE ZWAAGGREGATECALLEVENT (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER)"},
			func(a *sqlitetest.Table) {
				for i := int64(1); i <= 3; i++ {
					a.Insert(i, nil, int64(2))
				}
			}, "the table lacks column(s) ZINCOMING", true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, wal := buildCalls(c.over, func(tb callTables) {
				if a, ok := tb["ZWAAGGREGATECALLEVENT"]; ok {
					c.agg(a.t)
				}
				calls(tb)
			})
			res := runCallsErr(t, db, wal, "wal-frames-not-applied", "whatsapp-ios-join-undecided")
			if res.Err != nil {
				t.Fatalf("Parse: %v", res.Err)
			}
			recs := byRowid(res)
			for _, n := range []int64{1, 2} {
				raw := rawOf(recs[n])
				if raw["direction_state"] != "undecided" || recs[n].Payload["direction"] != "unknown" || !reflect.DeepEqual(raw["join_undecided"], []any{"ZWAAGGREGATECALLEVENT"}) {
					t.Errorf("call %d: %v", n, recs[n].Payload)
				}
			}
			// the count is the call table's own fact: two calls on one aggregate are multi-call whatever the aggregate table says,
			// and the row that could not be read is listed as undecided
			for _, n := range []int64{4, 5} {
				raw := rawOf(recs[n])
				if raw["direction_state"] != "multi-call-aggregate" || raw["aggregate_calls"] != int64(2) || raw["aggregate"] != nil || !reflect.DeepEqual(raw["join_undecided"], []any{"ZWAAGGREGATECALLEVENT"}) {
					t.Errorf("call %d: %v", n, recs[n].Payload)
				}
			}
			if raw := rawOf(recs[3]); raw["direction_state"] != "no-aggregate" || raw["join_undecided"] != nil {
				t.Errorf("call 3 names no aggregate, so no join is made: %v", raw)
			}
			if got := res.Notes["join_undecided.ZWAAGGREGATECALLEVENT"]; got != c.note {
				t.Errorf("note join_undecided.ZWAAGGREGATECALLEVENT = %q, want %q", got, c.note)
			}
			var schema, count int
			var order []string
			for _, w := range res.Warnings {
				order = append(order, w.Locator)
				if w.Locator == "sqlite:schema" && strings.Contains(w.Reason, "whatsapp-ios-join-undecided: table ZWAAGGREGATECALLEVENT: "+c.note) {
					schema++
				}
				if w.Locator == "sqlite:table=ZWAAGGREGATECALLEVENT" && w.Reason == counted {
					count++
				}
			}
			if count != 1 || (schema == 1) != c.schemaOK || (!c.schemaOK && schema != 0) {
				t.Errorf("schema warnings %d (want one: %v), counted %d: %v", schema, c.schemaOK, count, res.Warnings)
			}
			if len(order) == 0 || order[len(order)-1] != "sqlite:table=ZWAAGGREGATECALLEVENT" {
				t.Errorf("the counted warning follows every other: %v", order)
			}
		})
	}
}

// The artifact flags the host sets on partial captures are stated as notes before any mapping.
func TestIOSWhatsAppCallsIncompleteArtifactsAreNoted(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-callhistory"), "ios-callhistory")
	p := ioswacalls.Parser{}
	in := b.Input(h, p, false)
	in.Primary.Incomplete = true
	w := in.Artifacts["wal"]
	w.Incomplete = true
	in.Artifacts["wal"] = w
	res := runLenient(t, h, p, in, "wal-frames-not-applied")
	if res.Notes["incomplete.db"] != "true" || res.Notes["incomplete.wal"] != "true" {
		t.Errorf("notes %v", res.Notes)
	}
}

// The aggregate set is charged to the budget and given back: after a parse the budget holds nothing (PF4, as iosaddressbook).
func TestIOSWhatsAppCallsAggregateSetIsFreedFromTheBudget(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-callhistory"), "ios-callhistory")
	p := ioswacalls.Parser{}
	in := b.Input(h, p, false)
	bud := parse.NewBudget(256 << 20)
	in.Budget = bud.View()
	res := h.RunLenient(p, in)
	if res.Err != nil || len(res.Records) != 16 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	if used := bud.Used(); used != 0 {
		t.Errorf("the budget still holds %d bytes after Parse", used)
	}
}

// The aggregate cap: more distinct aggregates than the set may hold (a test seam lowers the cap to 10) leaves the keys read
// decided, every other row undecided, one warning with the counts, the records kept, and a job that returns
// ErrAggregatesIncomplete after the reader's own warnings.
func TestIOSWhatsAppCallsAggregateFloodIsIncomplete(t *testing.T) {
	const aggs, capKeys = 20, 10
	db, _ := buildCalls(nil, func(tb callTables) {
		for i := int64(1); i <= aggs; i++ {
			tb["ZWAAGGREGATECALLEVENT"].put(i, aggRow(int64(1), nil))
			tb["ZWACDCALLEVENT"].put(i, callRow(map[string]any{"Z1CALLEVENTS": i}))
		}
	})
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "flood", Files: map[string][]byte{"db": db}}.BundleIOSBackup(h, domainConsumer, dbName)
	p := cappedParser{max: capKeys}
	in := b.Input(h, p, false)
	bud := parse.NewBudget(256 << 20)
	in.Budget = bud.View()
	res := runLenientWith(t, h, p, in, "whatsapp-ios-aggregate-set-incomplete")
	if !errors.Is(res.Err, ioswacalls.ErrAggregatesIncomplete) {
		t.Fatalf("err %v, want ErrAggregatesIncomplete", res.Err)
	}
	recs := byRowid(res)
	if len(recs) != aggs {
		t.Fatalf("%d records, want all %d kept", len(recs), aggs)
	}
	for i := int64(1); i <= aggs; i++ {
		raw := rawOf(recs[i])
		if i <= capKeys && (recs[i].Payload["direction"] != "in" || raw["direction_state"] != nil) {
			t.Errorf("call %d (its aggregate was read): %v", i, recs[i].Payload)
		}
		if i > capKeys && (recs[i].Payload["direction"] != "unknown" || raw["direction_state"] != "undecided") {
			t.Errorf("call %d (its aggregate was not read): %v", i, recs[i].Payload)
		}
	}
	var warned []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "whatsapp-ios-aggregate-set-incomplete") {
			warned = append(warned, w.Locator+": "+w.Reason)
		}
	}
	if len(warned) != 1 || !strings.HasPrefix(warned[0], "sqlite:table=ZWACDCALLEVENT: whatsapp-ios-aggregate-set-incomplete: 20 rows, 10 read (") {
		t.Errorf("warnings %q", warned)
	}
	if res.Notes["aggregates"] != "10" {
		t.Errorf("note aggregates = %q, want the 10 keys read", res.Notes["aggregates"])
	}
	if bud.Used() != 0 {
		t.Errorf("the budget still holds %d bytes", bud.Used())
	}
	if n := len(res.Warnings); n == 0 || !strings.Contains(res.Warnings[n-1].Reason, "aggregate-set-incomplete") {
		t.Errorf("the set warning follows the reader's: %v", res.Warnings)
	}
}

// cappedParser is the parser with the aggregate cap lowered (the seam ParseWith).
type cappedParser struct {
	ioswacalls.Parser
	max int
}

func (c cappedParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return ioswacalls.ParseWith(ctx, in, out, c.max)
}
