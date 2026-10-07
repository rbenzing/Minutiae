package sqlitefile_test

// Live() and AsFound() with a rollback journal attached (plan 3I, Task 9). The
// cases come from the engine probe (oracle_journalrules_test.go): Live() is made
// to show what the engine shows after it rolls the journal back.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func attachJ(t testing.TB, db, journal []byte, o sqlitefile.Options) (*sqlitefile.DB, *sqlitefile.View, *sqlitefile.JournalInfo) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), o)
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal)))
	if err != nil {
		t.Fatalf("AttachJournal: %v", err)
	}
	return d, d.Live(), info
}

// tryRows reads table t of v; an error is returned, not fatal.
func tryRows(v *sqlitefile.View) ([][]any, error) {
	ctx := context.Background()
	tbl, err := v.Table(ctx, "t")
	if err != nil {
		return nil, err
	}
	var out [][]any
	err = tbl.Rows(ctx, func(r sqlitefile.Row) bool {
		vals := tbl.Resolve(r)
		out = append(out, []any{r.Rowid, anyOf(vals[0]), anyOf(vals[1])})
		return true
	})
	return out, err
}

func (j *jfix) viewLabels(t testing.TB, v *sqlitefile.View) string {
	t.Helper()
	rows, err := tryRows(v)
	if err != nil {
		return "error"
	}
	return j.labels(rows)
}

// buildCase returns the files of the named probe case.
func buildCase(t testing.TB, j *jfix, name string) (db, journal, wal []byte) {
	t.Helper()
	for _, c := range journalCases() {
		if c.name == name {
			return c.build(j, t.TempDir())
		}
	}
	t.Fatalf("no probe case %q", name)
	return nil, nil, nil
}

// wantLabels is the label string the engine showed for the case.
func wantLabels(name string) string { return strings.SplitN(journalWants[name], "|", 2)[0] }

func TestLiveAppliesHotJournal(t *testing.T) {
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	d, v, info := attachJ(t, j.dbAfter, journal, sqlitefile.Options{})
	if !info.Applied || info.AppliedRecords != uint32(len(j.changed)) || !info.Hot {
		t.Fatalf("%+v", info)
	}
	expectRows(t, "Live vs the before images", liveRows(t, v), j.before)
	if len(d.Journal().Records) != len(j.changed) {
		t.Fatalf("%d journal records", len(d.Journal().Records))
	}
	for i, pg := range j.changed {
		p, err := v.ReadPage(pg)
		if err != nil || !bytes.Equal(p.Data, j.s0.Page(pg)) {
			t.Fatalf("page %d: %v", pg, err)
		}
		rec := d.Journal().Records[i]
		if p.Loc.File != sqlitefile.FileJournal || p.Loc.Record != i || p.Loc.Offset != rec.Offset || !bytes.Equal(journal[p.Loc.Offset:p.Loc.Offset+ovPS], j.s0.Page(pg)) {
			t.Errorf("page %d: loc %+v, record %+v", pg, p.Loc, rec)
		}
	}
	// A row's location points at its cell in the journal file.
	tbl, err := v.Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	_ = tbl.Rows(context.Background(), func(r sqlitefile.Row) bool {
		l := r.Loc
		want := j.s0.Page(l.Page)[l.Offset-l.PageOffset : l.Offset-l.PageOffset+l.Length]
		if l.File != sqlitefile.FileJournal || !bytes.Equal(journal[l.Offset:l.Offset+l.Length], want) {
			t.Errorf("row %d: loc %+v", r.Rowid, l)
		}
		checked++
		return true
	})
	if checked != jrows {
		t.Errorf("%d rows", checked)
	}
	if !viewWarns(v, sqlitefile.WarnJournalHot, 0) {
		t.Errorf("journal-hot missing: %v", warnCodesOf(v))
	}
	expectRows(t, "engine", engineRowsOf(t, j, j.dbAfter, journal), j.before)
}

// engineRowsOf lets the engine roll back copies and returns its rows.
func engineRowsOf(t testing.TB, j *jfix, db, journal []byte) [][]any {
	t.Helper()
	return j.engineRows(t, db, journal, nil)
}

func TestAsFoundIsRawDBPlusWAL(t *testing.T) {
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	d, live, _ := attachJ(t, j.dbAfter, journal, sqlitefile.Options{})
	asFound := d.AsFound()
	expectRows(t, "AsFound is the file as found", liveRows(t, asFound), j.after)
	if equalRows(liveRows(t, asFound), liveRows(t, live)) {
		t.Error("AsFound equals Live although a hot journal is applied")
	}
	p, err := asFound.ReadPage(j.changed[0])
	if err != nil || p.Loc.File != sqlitefile.FileDB {
		t.Errorf("%v %+v", err, p.Loc)
	}
	// Without an applied journal AsFound equals Live.
	j2 := newJfix()
	b := j2.journal(512, -1).Bytes()
	b[0] = 0
	d2, live2, info := attachJ(t, j2.dbAfter, b, sqlitefile.Options{})
	if info.Applied {
		t.Fatal("applied")
	}
	expectRows(t, "no applied journal", liveRows(t, d2.AsFound()), liveRows(t, live2))
	// With a WAL attached it is the file plus the WAL overlay.
	db, jr, wal := buildCase(t, newJfix(), "hot journal and a WAL")
	d3, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d3.AttachJournal(bytes.NewReader(jr), int64(len(jr))); err != nil {
		t.Fatal(err)
	}
	if _, err := d3.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	jf := newJfix()
	if got := jf.viewLabels(t, d3.AsFound()); got != lab("W", 5, "A", 35) {
		t.Errorf("AsFound with a WAL: %s", got)
	}
	if got := jf.viewLabels(t, d3.Live()); got != wantLabels("hot journal and a WAL") {
		t.Errorf("Live with a WAL: %s", got)
	}
	if _, bad := findWarn(d3.AsFound(), sqlitefile.WarnJournalAndWAL); bad {
		t.Error("AsFound does not roll back: no journal-and-wal")
	}
}

func TestLiveRollbackStopsAtBadChecksum(t *testing.T) {
	j := newJfix()
	name := "bad checksum at record 1"
	db, journal, _ := buildCase(t, j, name)
	d, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels(name) {
		t.Errorf("labels %s, want %s", got, wantLabels(name))
	}
	if info.FirstBadRecord != 1 || info.AppliedRecords != 1 || !info.Applied {
		t.Errorf("%+v", info)
	}
	for i, r := range d.Journal().Records {
		if r.Applied != (i == 0) {
			t.Errorf("record %d applied %v", i, r.Applied)
		}
	}
}

func TestLiveRollbackTruncatesToInitialPages(t *testing.T) {
	j := newJfix()
	name := "record above the initial size in the middle (db longer)"
	db, journal, _ := buildCase(t, j, name)
	d, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	initial := j.s0.Pages()
	if got := j.viewLabels(t, v); got != wantLabels(name) {
		t.Errorf("labels %s", got)
	}
	if v.Info().PageCount != initial || v.Addressable() != initial {
		t.Errorf("page count %d addressable %d, want %d", v.Info().PageCount, v.Addressable(), initial)
	}
	if _, err := v.ReadPage(initial + 1); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("a page above the initial size: %v", err)
	}
	if _, err := d.RawPage(initial + 1); err != nil {
		t.Errorf("the file as found does hold page %d: %v", initial+1, err)
	}
	// the skipped record (above the initial size) is not applied
	if info.AppliedRecords != uint32(len(j.changed)) {
		t.Errorf("applied %d records, want %d", info.AppliedRecords, len(j.changed))
	}
	for _, r := range d.Journal().Records {
		if r.Page > initial && r.Applied {
			t.Errorf("record for page %d applied", r.Page)
		}
	}
}

func TestLiveRollbackLastDuplicateWins(t *testing.T) {
	j := newJfix()
	// junk then real: the real image wins
	db, journal, _ := buildCase(t, j, "duplicate page: junk then real")
	d, v, _ := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels("duplicate page: junk then real") {
		t.Errorf("labels %s", got)
	}
	n := 0
	for _, r := range d.Journal().Records {
		if r.Applied {
			n++
		}
	}
	if len(d.Journal().Records) == 0 || n != len(j.changed) || d.Journal().Records[0].Applied {
		t.Errorf("%d applied; the first junk record must be shadowed", n)
	}
	// real then junk: the junk image wins (the engine reads a malformed database)
	j2 := newJfix()
	db2, journal2, _ := buildCase(t, j2, "duplicate page: real then junk")
	_, v2, _ := attachJ(t, db2, journal2, sqlitefile.Options{})
	p, err := v2.ReadPage(j2.changed[0])
	if err != nil || !bytes.Equal(p.Data, junkPage()) || p.Loc.Record != len(j2.changed) {
		t.Errorf("page %d: %v loc %+v", j2.changed[0], err, p.Loc)
	}
}

func TestLiveRollbackPageZeroEndsPlayback(t *testing.T) {
	name := "record for page 0 in the middle"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	d, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels(name) || info.AppliedRecords != 2 {
		t.Errorf("labels %s, applied %d", got, info.AppliedRecords)
	}
	_ = d
}

func TestLiveRollbackLockBytePageEndsPlayback(t *testing.T) {
	name := "record for the lock-byte page in the middle"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels(name) || info.AppliedRecords != 2 {
		t.Errorf("labels %s, applied %d", got, info.AppliedRecords)
	}
	// the engine extends the file to the initial size; Live() never invents pages
	if v.Addressable() != j.s0.Pages() || !viewWarns(v, sqlitefile.WarnPageCountClamped, 0) {
		t.Errorf("addressable %d, warnings %v", v.Addressable(), warnCodesOf(v))
	}
}

func TestLiveRollbackSkipsPagesAboveInitialSize(t *testing.T) {
	name := "record above the initial size in the middle (db longer)"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels(name) || info.AppliedRecords != uint32(len(j.changed)) {
		t.Errorf("labels %s applied %d", got, info.AppliedRecords)
	}
}

func TestLiveRollbackZeroRecordsStillTruncates(t *testing.T) {
	name := "first header nRec 0, database longer than initial"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if got := j.viewLabels(t, v); got != wantLabels(name) || info.AppliedRecords != 0 || !info.Applied {
		t.Errorf("labels %s %+v", got, info)
	}
	if v.Addressable() != j.s0.Pages() || v.Info().PageCount != j.s0.Pages() {
		t.Errorf("addressable %d page count %d, want %d", v.Addressable(), v.Info().PageCount, j.s0.Pages())
	}
	if _, err := v.ReadPage(j.s0.Pages() + 1); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("page above the truncation: %v", err)
	}
}

func TestLiveInfoAfterRollback(t *testing.T) {
	j := newJfix()
	// the transaction changed the schema cookie, the user version and the free
	// page count: the file has the new values, the journaled page 1 the old
	db := bytes.Clone(j.dbAfter)
	binary.BigEndian.PutUint32(db[40:], 77)
	binary.BigEndian.PutUint32(db[60:], 9)
	p1 := bytes.Clone(j.s0.Page(1))
	binary.BigEndian.PutUint32(p1[40:], 5)
	binary.BigEndian.PutUint32(p1[60:], 3)
	jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
	jr.Record(1, p1)
	d, v, _ := attachJ(t, db, jr.Bytes(), sqlitefile.Options{})
	if in := v.Info(); in.SchemaCookie != 5 || in.UserVersion != 3 || in.PageCount != j.s0.Pages() {
		t.Errorf("Live info: %+v", in)
	}
	if in := d.Info(); in.SchemaCookie != 77 || in.UserVersion != 9 {
		t.Errorf("DB.Info must keep the as-found values: %+v", in)
	}
	if in := d.AsFound().Info(); in.SchemaCookie != 77 {
		t.Errorf("AsFound info: %+v", in)
	}
	e := j.engineDB(t, db, jr.Bytes())
	for pragma, want := range map[string]string{"schema_version": "5", "user_version": "3"} {
		if got := pragmaString(t, e, pragma); got != want {
			t.Errorf("engine %s = %s, want %s", pragma, got, want)
		}
	}
}

func TestLiveIgnoresNonHotJournal(t *testing.T) {
	for _, c := range []struct{ name, reason string }{
		{"first byte zero", "not-hot"},
		{"zeroed header (persist)", "zeroed-header"},
		{"magic mismatch, first byte non-zero", "header-invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := newJfix()
			db, journal, _ := buildCase(t, j, c.name)
			d, v, info := attachJ(t, db, journal, sqlitefile.Options{})
			if info.Applied || info.NotAppliedReason != c.reason {
				t.Errorf("%+v", info)
			}
			_, plain := openLive(t, db, sqlitefile.Options{})
			expectRows(t, "equals the no-journal view", liveRows(t, v), liveRows(t, plain))
			if got := j.viewLabels(t, v); got != wantLabels(c.name) {
				t.Errorf("labels %s", got)
			}
			if _, hot := findWarn(v, sqlitefile.WarnJournalHot); hot != (c.reason == "header-invalid") {
				t.Errorf("journal-hot present %v for reason %s", hot, c.reason)
			}
			_ = d
		})
	}
}

func TestLiveIgnoresJournalPageSizeMismatch(t *testing.T) {
	name := "journal page size 2048 against a 1024 database"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if info.Applied || info.NotAppliedReason != "page-size-mismatch" || info.PageSizeMatchesDB {
		t.Errorf("%+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnJournalPageSizeMismatch, 0) {
		t.Errorf("journal-page-size-mismatch missing: %v", warnCodesOf(v))
	}
	// deliberate: the engine rewrites the database with the journal's page size
	// and reads a malformed file; Live() shows the file as found
	if got := j.viewLabels(t, v); got != lab("A", 40) {
		t.Errorf("labels %s", got)
	}
}

func TestLiveIgnoresJournalWithUnknownSuperJournal(t *testing.T) {
	name := "super-journal named, file absent"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if info.Applied || info.NotAppliedReason != "super-journal-unknown" || !info.HasSuperJournal {
		t.Errorf("%+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnJournalSuperUnknown, 0) {
		t.Errorf("journal-super-unknown missing: %v", warnCodesOf(v))
	}
	if got := j.viewLabels(t, v); got != wantLabels(name) {
		t.Errorf("labels %s, the engine showed %s (the named file is absent)", got, wantLabels(name))
	}
}

func TestLiveAppliesJournalWhenSuperJournalDeclaredPresent(t *testing.T) {
	name := "super-journal named, file present"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	_, v, info := attachJ(t, db, journal, sqlitefile.Options{SuperJournalPresent: true})
	if !info.Applied || info.AppliedRecords != uint32(len(j.changed)) {
		t.Errorf("%+v", info)
	}
	if viewWarns(v, sqlitefile.WarnJournalSuperUnknown, 0) {
		t.Error("journal-super-unknown although declared present")
	}
	if got := j.viewLabels(t, v); got != wantLabels(name) {
		t.Errorf("labels %s", got)
	}
}

func TestLiveRollbackWithWAL(t *testing.T) {
	name := "hot journal and a WAL"
	for _, journalFirst := range []bool{true, false} {
		j := newJfix()
		db, journal, wal := buildCase(t, j, name)
		d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		attJ := func() {
			if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
				t.Fatal(err)
			}
		}
		attW := func() {
			if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
				t.Fatal(err)
			}
		}
		if journalFirst {
			attJ()
			attW()
		} else {
			attW()
			attJ()
		}
		v := d.Live()
		if got := j.viewLabels(t, v); got != wantLabels(name) {
			t.Errorf("journalFirst=%v: labels %s, want %s", journalFirst, got, wantLabels(name))
		}
		if !viewWarns(v, sqlitefile.WarnJournalAndWAL, 0) {
			t.Errorf("journal-and-wal missing: %v", warnCodesOf(v))
		}
	}
}

func TestAttachJournalAfterLiveSnapshot(t *testing.T) {
	j := newJfix()
	d, err := sqlitefile.Open(bytes.NewReader(j.dbAfter), int64(len(j.dbAfter)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v0 := d.Live()
	journal := j.journal(512, -1).Bytes()
	if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	expectRows(t, "a view taken before the attach", liveRows(t, v0), j.after)
	expectRows(t, "a view taken after", liveRows(t, d.Live()), j.before)
}

func TestAttachJournalTwice(t *testing.T) {
	j := newJfix()
	d, _, _ := attachJ(t, j.dbAfter, j.journal(512, -1).Bytes(), sqlitefile.Options{})
	b := j.journal(512, -1).Bytes()
	if _, err := d.AttachJournal(bytes.NewReader(b), int64(len(b))); !errors.Is(err, sqlitefile.ErrAlreadyAttached) {
		t.Errorf("second attach: %v", err)
	}
	if st := d.Status(); st.Journal == nil || !st.Journal.Applied {
		t.Errorf("status %+v", st)
	}
}

// TestBuilderJournalMatchesEngine: the first test that breaks the builder/reader
// circle for journals: a builder database plus a builder journal, rolled back by
// the engine, equals Live() and differs from AsFound().
func TestBuilderJournalMatchesEngine(t *testing.T) {
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	d, v, _ := attachJ(t, j.dbAfter, journal, sqlitefile.Options{})
	engine := engineRowsOf(t, j, j.dbAfter, journal)
	expectRows(t, "engine vs Live", engine, liveRows(t, v))
	if equalRows(engine, liveRows(t, d.AsFound())) {
		t.Error("the engine rolled back but AsFound shows the same rows")
	}
}

// TestLiveAgreesWithEngineOnEveryProbeRow runs every row of the engine probe
// through Live(): same rows, same page count. The deliberate deviations are
// named here.
func TestLiveAgreesWithEngineOnEveryProbeRow(t *testing.T) {
	for _, c := range journalCases() {
		t.Run(c.name, func(t *testing.T) {
			j := newJfix()
			db, journal, wal := c.build(j, t.TempDir())
			outcome := j.runEngine(t, db, journal, wal)
			opts := sqlitefile.Options{SuperJournalPresent: strings.Contains(c.name, "file present")}
			d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), opts)
			if err != nil {
				t.Fatal(err)
			}
			if journal != nil {
				if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
					t.Fatal(err)
				}
			}
			if wal != nil {
				if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
					t.Fatal(err)
				}
			}
			v := d.Live()
			got := j.viewLabels(t, v)
			switch {
			case strings.HasPrefix(c.name, "journal page size 2048"):
				// deviation: the engine adopts the journal's page size and reads garbage;
				// Live() does not apply the journal and warns
				if got != lab("A", 40) {
					t.Errorf("labels %s", got)
				}
				return
			case outcome.err != "" && c.name != "initial pages 0":
				return // the engine reads a malformed file; Live() only has to survive
			case c.name == "initial pages 0":
				if _, err := tryRows(v); err == nil {
					t.Error("a database truncated to zero pages still has table t")
				}
			default:
				if got != outcome.labels {
					t.Errorf("Live labels %s, engine %s", got, outcome.labels)
				}
			}
			// deviation: the engine extends the file to the initial size with zeros
			if !strings.Contains(c.name, "lock-byte") && int64(v.Info().PageCount) != outcome.pages {
				t.Errorf("page count %d, the engine's file has %d pages", v.Info().PageCount, outcome.pages)
			}
		})
	}
}
