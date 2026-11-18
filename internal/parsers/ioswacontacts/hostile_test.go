package ioswacontacts_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The hostile rows of spec 10 that earlier tests do not cover. Each test builds its database here, runs the parser through the
// harness and requires: no panic, no invented value, and a stated warning or raw marker. A bound is a typed error or a counted
// warning, never a silent cut.

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
	p := ioswacontacts.Parser{}
	in := b.Input(h, p, false)
	if budget > 0 {
		in.Budget = parse.NewBudget(budget).View()
	}
	return h.RunLenient(p, in)
}

// typedOutcome fails the test on an error that is not one of the parser's or the decoder's typed answers to a hostile file.
func typedOutcome(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil || parsertest.HostileOK(err) || errors.Is(err, ioswacontacts.ErrNoWhatsAppTables) {
		return
	}
	t.Fatalf("%s: untyped error %v", label, err)
}

// padName is a distinct name long enough that a few hundred rows span many pages.
func padName(i int64) string { return fmt.Sprintf("c%d %s", i, strings.Repeat("x", 60)) }

func manyContacts(rows int64) func(put putRow) {
	return func(put putRow) {
		for i := int64(1); i <= rows; i++ {
			put(i, contactRow(map[string]any{"ZFULLNAME": padName(i), "ZWHATSAPPID": fmt.Sprintf("1555%07d@s.whatsapp.net", i)}))
		}
	}
}

// A database cut inside the table: the pages that are gone are stated by the decoder as warnings, and nothing is guessed about
// the rows that were lost. Every cut length holds the contract (typed errors only, no refused record, no record the database did
// not hold), a cut that loses rows is never an empty complete result, and a warning of the reader is raised once.
func TestIOSWhatsAppContactsTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 600
	full, _ := buildContacts(nil, manyContacts(rows))
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
			if n, _ := r.Payload["display_name"].(string); !strings.HasPrefix(n, fmt.Sprintf("c%d ", rowidOf(r.Locator))) {
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

// damagedStore is a store of 300 contacts whose last leaf has an invalid page type: the reader states the damage.
func damagedStore(t *testing.T) []byte {
	t.Helper()
	const rows = 300
	b, _ := contactBuilder(4096, nil, manyContacts(rows))
	data := slices.Clone(b.Bytes())
	off := strings.LastIndex(string(data), fmt.Sprintf("c%d x", rows))
	if off < 0 {
		t.Fatal("the last row is not in the store")
	}
	data[off/4096*4096] = 0
	return data
}

// A damaged leaf of the table: the reader's warning is stated (once, after the rows' own warnings: PF13a), the other rows are
// kept, and no row the database did not hold appears.
func TestIOSWhatsAppContactsDamagedLeafIsStatedOnce(t *testing.T) {
	good, _ := buildContacts(nil, manyContacts(300))
	if r := hostileRun(t, good, nil, nil, 0); r.Err != nil || len(r.Records) != 300 {
		t.Fatalf("control: err %v, %d records", r.Err, len(r.Records))
	}
	res := hostileRun(t, damagedStore(t), nil, nil, 0)
	typedOutcome(t, "damaged leaf", res.Err)
	if len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("violations %v refused %d", res.Violations, res.Refused)
	}
	if len(res.Records) == 0 || len(res.Records) >= 300 {
		t.Fatalf("%d records of 300: the damage should lose some and keep others", len(res.Records))
	}
	readers := 0
	seen := map[string]bool{}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Locator, "sqlite:file=") {
			readers++
			if seen[w.Locator+w.Reason] {
				t.Errorf("the reader's warning %v is raised twice", w)
			}
			seen[w.Locator+w.Reason] = true
		}
	}
	if readers == 0 {
		t.Errorf("no reader warning for the damaged page: %+v", res.Warnings)
	}
}

// A hot journal rolls the interrupted rewrite back: the row states what the database held before it.
func TestIOSWhatsAppContactsHotJournalIsRolledBack(t *testing.T) {
	const ps = 512
	b, tb := contactBuilder(ps, nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "original", "ZWHATSAPPID": "15550000001@s.whatsapp.net"}))
	})
	snap := b.Snapshot()
	tb.Update(1, nil, int64(1), int64(1), nil, nil, "interrupted", nil, nil, nil, nil, nil, nil, "15550000001@s.whatsapp.net")
	root := tb.Root()
	db := b.Bytes()
	j := b.NewJournal(ps, 0x5eed0003, snap.Pages())
	j.Record(root, snap.Page(root))
	res := hostileRun(t, db, nil, j.Bytes(), 0)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	var names []any
	for _, r := range res.Records {
		names = append(names, r.Payload["display_name"])
	}
	if len(names) != 1 || names[0] != "original" {
		t.Errorf("display names %q, want only the rolled-back state", names)
	}
	hot := false
	for _, w := range res.Warnings {
		hot = hot || strings.Contains(w.Reason, "journal-hot")
	}
	if !hot {
		t.Errorf("no journal-hot warning: %+v", res.Warnings)
	}
}

// Plan 4I2 Task 9: contact 14 of ios-contactsv2 lies only in a committed WAL frame and contact 16 only in an uncommitted one.
// The record of contact 14 points into the WAL artifact (its artifact id, path and range), and 16 is not a record at all.
func TestIOSWhatsAppContactsWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-contactsv2"), "ios-contactsv2")
	wal, ok := b.ByRole["wal"]
	if !ok || wal.ID == "" || wal.ID == b.Primary.ID || wal.Source.RemotePath == b.Primary.Source.RemotePath {
		t.Fatalf("the bundle has no distinct WAL artifact: %+v", wal)
	}
	p := ioswacontacts.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), rowWarnings...)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	byRow := byRowid(res)
	r14, has14 := byRow[14]
	if !has14 {
		t.Fatal("contact 14 (committed WAL frame) is no record")
	}
	if r14.ArtifactID != wal.ID || r14.SourcePath != wal.Source.RemotePath || r14.Range == nil || r14.Range.Offset+r14.Range.Length > wal.Size {
		t.Errorf("contact 14: artifact %s path %s range %+v, want the WAL artifact %s at %s", r14.ArtifactID, r14.SourcePath, r14.Range, wal.ID, wal.Source.RemotePath)
	}
	if _, has := byRow[16]; has {
		t.Error("contact 16 lies only in an uncommitted frame and is a record")
	}
	// PF13a: the row warnings come first and the reader's warning last
	if n := len(res.Warnings); n < 2 || !strings.Contains(res.Warnings[n-1].Reason, "wal-frames-not-applied") || !strings.HasPrefix(res.Warnings[n-1].Locator, "sqlite:file=") {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	for _, w := range res.Warnings[:len(res.Warnings)-1] {
		if strings.HasPrefix(w.Locator, "sqlite:file=") {
			t.Errorf("a reader warning %v before the last row warning", w)
		}
	}
}

// Wrong-class and hostile cells in every column the mapping reads never panic, never invent a value and are stated.
func TestIOSWhatsAppContactsWrongClassCellsAreStated(t *testing.T) {
	cols := []string{"Z_ENT", "ZABOUTTIMESTAMP", "ZABOUTTEXT", "ZFULLNAME", "ZGIVENNAME", "ZHIGHLIGHTEDNAME", "ZIDENTIFIER", "ZLID", "ZPHONENUMBER", "ZPHONENUMBERLABEL", "ZWHATSAPPID"}
	db, _ := buildContacts(nil, func(put putRow) {
		id := int64(0)
		for _, v := range []any{int64(7), 1.5, []byte{1, 2, 3}} {
			for _, c := range cols {
				id++
				kv := map[string]any{"ZFULLNAME": "Ok", "ZWHATSAPPID": "15550000001@s.whatsapp.net", "ZPHONENUMBER": "+1", c: v}
				put(id, contactRow(kv))
			}
		}
	})
	res := runContactsErr(t, db, nil, "ZWAADDRESSBOOKCONTACT row ")
	if res.Err != nil || res.Refused != 0 || len(res.Records) == 0 {
		t.Fatalf("err %v refused %d records %d", res.Err, res.Refused, len(res.Records))
	}
	for _, r := range res.Records {
		if r.Time != nil || r.Recovery != "" {
			t.Errorf("%s: time %v recovery %q", r.Locator, r.Time, r.Recovery)
		}
	}
}

// The notes of the identity (container, app) are written before any mapping, so they survive the host's cap on notes, where
// the first write of a key wins and the later notes are the ones dropped. The cap is the real 64 and then lowered so that it
// bites on this fixture.
func TestIOSWhatsAppContactsNotesSurviveTheNoteCap(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-contactsv2-nolid")
	run := func(limit int) parsertest.Result {
		h := parsertest.NewHarness(t)
		p := ioswacontacts.Parser{}
		in := bundleOf(h, fx, "ios-contactsv2-nolid").Input(h, p, false)
		in.Limits.MaxNotes = limit
		return h.RunLenient(p, in)
	}
	want := map[string]string{"container": "backup_domain", "app": "whatsapp", "app_basis": "backup_domain"}
	full := run(parse.DefaultLimits().MaxNotes)
	if full.Err != nil || len(full.Notes) > 64 || len(full.Notes) < len(want)+1 || full.Notes["ZLID"] != "absent" {
		t.Fatalf("err %v, %d notes %v", full.Err, len(full.Notes), full.Notes)
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

func TestIOSWhatsAppContactsIncompleteArtifactsAreNoted(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-contactsv2"), "ios-contactsv2")
	p := ioswacontacts.Parser{}
	in := b.Input(h, p, false)
	in.Primary.Incomplete = true
	w := in.Artifacts["wal"]
	w.Incomplete = true
	in.Artifacts["wal"] = w
	res := runLenient(t, h, p, in, rowWarnings...)
	if res.Notes["incomplete.db"] != "true" || res.Notes["incomplete.wal"] != "true" {
		t.Errorf("notes %v", res.Notes)
	}
}

// The warnings are bounded by the runner's cap of 1000, and the rows beyond it are counted, never silently lost: 1100 group rows
// give 1000 warnings and the note unmapped_rows names all 1100 (the mapping is total).
func TestIOSWhatsAppContactsRowWarningsAreCappedAndCounted(t *testing.T) {
	const rows = 1100
	db, _ := buildContacts(nil, func(put putRow) {
		for i := int64(1); i <= rows; i++ {
			put(i, contactRow(map[string]any{"ZFULLNAME": "g", "ZWHATSAPPID": fmt.Sprintf("%d@g.us", i)}))
		}
	})
	res := hostileRun(t, db, nil, nil, 0)
	if res.Err != nil || len(res.Records) != 0 || len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("err %v, %d records, violations %v, refused %d", res.Err, len(res.Records), res.Violations, res.Refused)
	}
	if len(res.Warnings) != 1000 || res.Notes["unmapped_rows.ZWAADDRESSBOOKCONTACT"] != fmt.Sprint(rows) {
		t.Errorf("%d warnings, unmapped_rows %q, want 1000 and %d", len(res.Warnings), res.Notes["unmapped_rows.ZWAADDRESSBOOKCONTACT"], rows)
	}
}

// Reader warnings and row warnings share the one cap and one warnings_dropped total: a store with a damaged leaf and 1100 group
// rows states 1000 warnings (the rows' own, which come first) and the reader's warnings that no longer fit are the one total.
func TestIOSWhatsAppContactsSharedWarningCapStatesOneTotal(t *testing.T) {
	const rows = 1100
	b, _ := contactBuilder(4096, nil, func(put putRow) {
		for i := int64(1); i <= rows; i++ {
			put(i, contactRow(map[string]any{"ZFULLNAME": padName(i), "ZWHATSAPPID": fmt.Sprintf("%d@g.us", i)}))
		}
	})
	data := slices.Clone(b.Bytes())
	off := strings.LastIndex(string(data), fmt.Sprintf("c%d x", rows))
	data[off/4096*4096] = 0
	res := hostileRun(t, data, nil, nil, 0)
	typedOutcome(t, "damaged group rows", res.Err)
	if len(res.Warnings) != 1000 || len(res.Violations) != 0 {
		t.Fatalf("%d warnings, violations %v", len(res.Warnings), res.Violations)
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Locator, "sqlite:file=") {
			t.Errorf("a reader warning %v inside a full cap", w)
		}
	}
	if n, err := strconv.Atoi(res.Notes["warnings_dropped"]); err != nil || n < 1 {
		t.Errorf("warnings_dropped %q, want the count of the reader's warnings that did not fit", res.Notes["warnings_dropped"])
	}
}
