package sqlitefile_test

// Tests from the Task 8 review (step 0c): the Live() overlay boundaries that
// no test pinned. Each scenario that says what the engine shows is compared
// with the engine on a copy of the files.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func findWarn(v *sqlitefile.View, code string) (sqlitefile.Warning, bool) {
	for _, w := range v.Warnings() {
		if w.Code == code {
			return w, true
		}
	}
	return sqlitefile.Warning{}, false
}

func countWarns(v *sqlitefile.View, code string) int {
	n := 0
	for _, w := range v.Warnings() {
		if w.Code == code {
			n++
		}
	}
	return n
}

// TestLiveDBSizeLatestCommitWins (I1): a commit that grows the database and a
// later commit that shrinks it: the size of the LATEST commit frame is the
// database size, not the largest one.
func TestLiveDBSizeLatestCommitWins(t *testing.T) {
	f := newWfix(20)
	db := f.b.Bytes()
	s0 := f.b.Snapshot()
	w := f.b.NewWAL(false, 1, 2, 0)
	for id := int64(21); id <= 80; id++ {
		f.set(id, byte(id), true)
	}
	f.b.CommitTo(w, s0)
	big := f.b.Snapshot()
	for id := int64(41); id <= 80; id++ {
		f.del(id)
	}
	f.b.CommitTo(w, big)
	final := f.b.Snapshot().Pages()
	if final >= big.Pages() || final <= s0.Pages() {
		t.Fatalf("scenario: %d pages, grown to %d, started at %d", final, big.Pages(), s0.Pages())
	}
	v := sameAsEngine(t, "grow then shrink", db, w.Bytes(), sqlitefile.Options{})
	expectRows(t, "grow then shrink vs builder", liveRows(t, v), f.want())
	if v.Addressable() != final || v.Info().PageCount != final {
		t.Errorf("addressable %d, page count %d, want the latest commit's %d (not the largest, %d)", v.Addressable(), v.Info().PageCount, final, big.Pages())
	}
	d, _, _ := attachLive(t, walMode(db), w.Bytes(), sqlitefile.Options{})
	if got := d.WAL().Info.DBPagesAfterCommit; got != final {
		t.Errorf("DBPagesAfterCommit %d, want %d", got, final)
	}
}

// TestLiveCommittedFrameBeatsALaterUncommittedFrameOfTheSamePage (I2): the
// overlay serves the newest COMMITTED frame of a page, never a later frame of
// the same page that no commit seals.
func TestLiveCommittedFrameBeatsALaterUncommittedFrameOfTheSamePage(t *testing.T) {
	f := newWfix(40)
	db := f.b.Bytes()
	s0 := f.b.Snapshot()
	w := f.b.NewWAL(false, 1, 2, 0)
	f.set(1, 101, false)
	f.b.CommitTo(w, s0)
	pages := changedPages(s0, f.b)
	junk := bytes.Repeat([]byte{0xEE}, ovPS)
	w.Frame(pages[0], junk, 0) // the same page again, never committed
	v := sameAsEngine(t, "committed beats uncommitted", db, w.Bytes(), sqlitefile.Options{})
	expectRows(t, "committed beats uncommitted vs builder", liveRows(t, v), f.want())
	p, err := v.ReadPage(pages[0])
	if err != nil || p.Loc.File != sqlitefile.FileWAL || p.Loc.Frame != 1 || bytes.Equal(p.Data, junk) {
		t.Errorf("page %d: %v %+v", pages[0], err, p.Loc)
	}
}

// TestLiveWALModeMismatchByHeaderBytes (I3): the warning is raised unless both
// header bytes say WAL (2/2), whichever one is wrong, and is labelled with the
// WAL file; the engine applies the log in every case.
func TestLiveWALModeMismatchByHeaderBytes(t *testing.T) {
	_, db, w := twoTxns(t)
	wal := w.Bytes()
	for _, c := range []struct {
		wv, rv byte
		warn   bool
	}{{1, 1, true}, {1, 2, true}, {2, 1, true}, {2, 2, false}} {
		t.Run(fmt.Sprintf("%d/%d", c.wv, c.rv), func(t *testing.T) {
			d := bytes.Clone(db)
			d[18], d[19] = c.wv, c.rv
			_, v, _ := attachLive(t, d, wal, sqlitefile.Options{})
			x, ok := findWarn(v, sqlitefile.WarnWALModeMismatch)
			if ok != c.warn {
				t.Fatalf("warning present %v, want %v", ok, c.warn)
			}
			if ok && x.File != sqlitefile.FileWAL {
				t.Errorf("File %v, want the WAL", x.File)
			}
			expectRows(t, "engine vs live", engineAsIs(t, d, wal), liveRows(t, v))
		})
	}
}

// TestLivePage1InWALFitChecks (I4): a WAL page 1 is adopted only when it fits
// the database (page size and reserved bytes agree, the header parses); one
// that does not fit leaves the database's header in place with a
// wal-page1-mismatch warning on page 1 of the WAL; the parse warnings of a page
// 1 that does fit are forwarded, labelled with the WAL.
func TestLivePage1InWALFitChecks(t *testing.T) {
	build := func(patch func(p1 []byte)) (*sqlitefile.DB, *sqlitefile.View) {
		f := newWfix(20)
		db := walMode(f.b.Bytes())
		p1 := bytes.Clone(db[:ovPS])
		binary.BigEndian.PutUint32(p1[40:], 77) // schema cookie
		patch(p1)
		w := f.b.NewWAL(false, 1, 2, 0)
		w.Frame(1, p1, f.b.Snapshot().Pages())
		d, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
		return d, v
	}
	for _, c := range []struct {
		name  string
		patch func(p1 []byte)
	}{
		{"reserved bytes differ", func(p1 []byte) { p1[20] = 8 }},
		{"page size field invalid", func(p1 []byte) { binary.BigEndian.PutUint16(p1[16:], 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, v := build(c.patch)
			x, ok := findWarn(v, sqlitefile.WarnWALPage1Mismatch)
			if !ok || x.File != sqlitefile.FileWAL || x.Page != 1 {
				t.Fatalf("wal-page1-mismatch on WAL page 1 missing: %v", v.Warnings())
			}
			if v.Info().SchemaCookie == 77 || v.Info().Reserved != d.Info().Reserved || v.Info().PageSize != d.Info().PageSize {
				t.Errorf("the database's header must be kept: %+v", v.Info())
			}
		})
	}
	t.Run("a fitting page 1 forwards its parse warnings", func(t *testing.T) {
		_, v := build(func(p1 []byte) { binary.BigEndian.PutUint32(p1[56:], 9) }) // encoding field out of range
		x, ok := findWarn(v, sqlitefile.WarnHdrEncodingInvalid)
		if !ok || x.File != sqlitefile.FileWAL || x.Page != 1 {
			t.Fatalf("hdr-encoding-invalid from the WAL page 1 missing: %v", v.Warnings())
		}
		if v.Info().SchemaCookie != 77 {
			t.Errorf("a fitting page 1 is adopted: cookie %d", v.Info().SchemaCookie)
		}
		if _, bad := findWarn(v, sqlitefile.WarnWALPage1Mismatch); bad {
			t.Error("wal-page1-mismatch for a page 1 that fits")
		}
	})
}

// walGapScenario returns a database of dbPages pages and a WAL whose single
// commit frame supplies page dbPages+3 and claims dbPages+5.
func walGapScenario() (db, wal []byte, dbPages uint32) {
	f := newWfix(20)
	db = walMode(f.b.Bytes())
	dbPages = f.b.Snapshot().Pages()
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(dbPages+3, bytes.Clone(f.b.PageBytes(2)), dbPages+5)
	return db, w.Bytes(), dbPages
}

// TestLivePagesUnavailableWarnsAtAttach (M1, M2): pages Live() cannot supply
// are said once at attach level, not only when a walker happens to hit them:
// committed frames that do not lie wholly inside the WAL (File=WAL) and gap
// pages below the commit's size that no frame and no database page supplies
// (File=DB). Nothing is zero-filled.
func TestLivePagesUnavailableWarnsAtAttach(t *testing.T) {
	t.Run("gap pages", func(t *testing.T) {
		db, wal, dbPages := walGapScenario()
		_, v, _ := attachLive(t, db, wal, sqlitefile.Options{})
		x, ok := findWarn(v, sqlitefile.WarnLivePagesUnavailable)
		if !ok || x.File != sqlitefile.FileDB || countWarns(v, sqlitefile.WarnLivePagesUnavailable) != 1 {
			t.Fatalf("one live-pages-unavailable warning on the database expected: %v", v.Warnings())
		}
		if !strings.Contains(x.Msg, "2 pages") || !strings.Contains(x.Msg, fmt.Sprint(dbPages+1)) {
			t.Errorf("the message must count the 2 gap pages and name the first (%d): %q", dbPages+1, x.Msg)
		}
		if _, err := v.ReadPage(dbPages + 1); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("gap page: %v", err)
		}
	})
	t.Run("frames past the end of the WAL", func(t *testing.T) {
		// A log of 512-byte pages under a 1024-byte database: the engine reads the
		// database page size from each frame, so the last frame does not hold a
		// whole page inside the file.
		f := newWfix(20)
		db := walMode(f.b.Bytes())
		w := sqlitetest.New(sqlitetest.Options{PageSize: 512}).NewWAL(false, 1, 2, 0)
		for i := range 3 {
			w.Frame(uint32(2+i), pageOf(512, byte(i)), 0)
		}
		w.Frame(5, pageOf(512, 9), f.b.Snapshot().Pages())
		_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
		x, ok := findWarn(v, sqlitefile.WarnLivePagesUnavailable)
		if !ok || x.File != sqlitefile.FileWAL || countWarns(v, sqlitefile.WarnLivePagesUnavailable) != 1 {
			t.Fatalf("one live-pages-unavailable warning on the WAL expected: %v", v.Warnings())
		}
		if !strings.Contains(x.Msg, "1 committed frame") || !strings.Contains(x.Msg, "slot 4") {
			t.Errorf("the message must count the frames and name the first slot: %q", x.Msg)
		}
	})
	t.Run("a complete WAL is silent", func(t *testing.T) {
		_, db, w := twoTxns(t)
		_, v, _ := attachLive(t, walMode(db), w.Bytes(), sqlitefile.Options{})
		if _, bad := findWarn(v, sqlitefile.WarnLivePagesUnavailable); bad {
			t.Errorf("warned about a complete log: %v", v.Warnings())
		}
	})
}

// TestWalkerWarningsNameTheFileTheyCameFrom (M1): a damaged cell on a page that
// the WAL supplies is a warning about the WAL, not the database file.
func TestWalkerWarningsNameTheFileTheyCameFrom(t *testing.T) {
	f := newWfix(40)
	db := walMode(f.b.Bytes())
	s0 := f.b.Snapshot()
	f.set(1, 101, false)
	leaf := changedPages(s0, f.b)[0]
	pg := bytes.Clone(f.b.PageBytes(leaf))
	pg[8+2], pg[8+3] = 0xff, 0xff // a cell pointer far outside the page
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(leaf, pg, s0.Pages())
	_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	tbl, err := v.Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = tbl.Rows(context.Background(), func(sqlitefile.Row) bool { return true })
	x, ok := findWarn(v, sqlitefile.WarnCellPointer)
	if !ok || x.Page != leaf || x.File != sqlitefile.FileWAL {
		t.Errorf("cell-pointer on page %d from the WAL expected: %v", leaf, v.Warnings())
	}
}

// TestUsedByLiveNeedsOnlyAValidHeader (M3): a log with a valid header and no
// commit is attached (UsedByLive) and changes nothing.
func TestUsedByLiveNeedsOnlyAValidHeader(t *testing.T) {
	f := newWfix(20)
	db := walMode(f.b.Bytes())
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(2, bytes.Clone(f.b.PageBytes(2)), 0)
	d, v, info := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	if !info.HeaderValid || !info.UsedByLive || info.LastCommit != 0 {
		t.Errorf("%+v", info)
	}
	_, plain := openLive(t, db, sqlitefile.Options{})
	expectRows(t, "no commit", liveRows(t, v), liveRows(t, plain))
	if !d.WAL().Info.UsedByLive {
		t.Error("the stored scan lost UsedByLive")
	}
}

// TestLiveOverflowChainAcrossTheWALOverlay (M4): the overflow pages of a row lie
// in the database file (the lower source of the overlay) and are followed
// through it; a chain page above the size the last commit left is not in the
// file (as for the engine) and is said so.
func TestLiveOverflowChainAcrossTheWALOverlay(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: ovPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, id, longText(100, byte(id)))
	}
	blob := bytes.Repeat([]byte("overflow-chain-"), 400) // about 6 KiB: several overflow pages
	tb.Insert(4, int64(4), string(blob))
	db := walMode(b.Bytes())
	pages := b.Snapshot().Pages()
	ov := tb.Overflow(4)
	if len(ov) < 3 {
		t.Fatalf("overflow chain of %d pages", len(ov))
	}
	read := func(v *sqlitefile.View) (sqlitefile.Value, bool) {
		var out sqlitefile.Value
		got := false
		tbl, err := v.Table(context.Background(), "t")
		if err != nil {
			t.Fatal(err)
		}
		_ = tbl.Rows(context.Background(), func(r sqlitefile.Row) bool {
			if r.Rowid == 4 {
				out, got = tbl.Resolve(r)[1], true
			}
			return true
		})
		return out, got
	}
	// A commit that rewrites page 1 only; the chain stays in the database.
	w := b.NewWAL(false, 1, 2, 0)
	w.Frame(1, bytes.Clone(db[:ovPS]), pages)
	_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	val, ok := read(v)
	if s, _ := val.Text(); !ok || s != string(blob) || val.Omitted {
		t.Errorf("the chain through the lower source was not read: ok %v len %d omitted %v warnings %v", ok, len(val.Bytes), val.Omitted, v.Warnings())
	}
	// The commit leaves fewer pages than the chain needs.
	maxOv := slicesMax(ov)
	w2 := b.NewWAL(false, 1, 2, 0)
	w2.Frame(1, bytes.Clone(db[:ovPS]), maxOv-1)
	_, v2, _ := attachLive(t, db, w2.Bytes(), sqlitefile.Options{})
	read(v2)
	found := false
	for _, x := range v2.Warnings() {
		if x.Code == sqlitefile.WarnPageRange && x.Page > v2.Addressable() {
			found = true
		}
	}
	if !found {
		t.Errorf("a chain page above the commit's size is outside the database: %v", v2.Warnings())
	}
}

func slicesMax(s []uint32) uint32 {
	m := s[0]
	for _, x := range s {
		m = max(m, x)
	}
	return m
}

// TestAttachWALInfoIsACopy (M5): the info AttachWAL returns and the one Status
// returns do not alias the stored scan.
func TestAttachWALInfoIsACopy(t *testing.T) {
	_, db, w := twoTxns(t)
	d, _, info := attachLive(t, walMode(db), w.Bytes(), sqlitefile.Options{})
	if len(info.Generations) == 0 {
		t.Fatal("no generations")
	}
	want := d.WAL().Info.Generations[0].Slots
	info.Generations[0].Slots = 12345
	if got := d.WAL().Info.Generations[0].Slots; got != want {
		t.Errorf("AttachWAL returned the stored Generations: %d", got)
	}
	st := d.Status()
	st.WAL.Generations[0].Slots = 54321
	if got := d.WAL().Info.Generations[0].Slots; got != want {
		t.Errorf("Status returned the stored Generations: %d", got)
	}
}

// TestWALNotAppliedWarningLayout (M6): the exact text of the one
// wal-frames-not-applied warning: classes in the order broken, detached, stale,
// uncommitted, separated by ", ", each with its count and first slot.
func TestWALNotAppliedWarningLayout(t *testing.T) {
	// slots: 1 committed, 2 uncommitted, 3 broken, 4 detached, 5 stale (salt flipped, then detached)
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 3}, {3, 0}, {4, 0}, {5, 0}, {6, 0}})
	orig := w.Bytes()[32+2*(24+512)+24+10]
	w.PatchFrame(3, 24+10, orig^0x04) // slot 3 broken
	w.PatchFrame(5, 11, 0xee)         // slot 5: another generation
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	var msgs []string
	for _, x := range s.Warnings {
		if x.Code == sqlitefile.WarnWALFramesNotApplied {
			msgs = append(msgs, x.Msg)
		}
	}
	want := "frames the engine does not apply: broken 1 (first slot 3), detached 1 (first slot 4), stale 1 (first slot 5), uncommitted 1 (first slot 2)"
	if len(msgs) != 1 || msgs[0] != want {
		t.Errorf("messages %q, want %q", msgs, want)
	}
	// A class with no frames says 0.
	s = scanBytes(t, buildWAL(512, false, 1, 2, []walSpec{{2, 3}, {3, 0}}).Bytes(), 512, sqlitefile.Options{})
	for _, x := range s.Warnings {
		if x.Code == sqlitefile.WarnWALFramesNotApplied && x.Msg != "frames the engine does not apply: broken 0, detached 0, stale 0, uncommitted 1 (first slot 2)" {
			t.Errorf("%q", x.Msg)
		}
	}
}

// TestLiveInfoFileSizeIsTheDatabaseFile (M7): after a WAL page 1 is adopted,
// Info.FileSize is still the size of the database file, not the pages the log
// adds.
func TestLiveInfoFileSizeIsTheDatabaseFile(t *testing.T) {
	f := newWfix(20)
	db := walMode(f.b.Bytes())
	dbPages := f.b.Snapshot().Pages()
	p1 := bytes.Clone(db[:ovPS])
	binary.BigEndian.PutUint32(p1[40:], 77)
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(1, p1, 0)
	w.Frame(dbPages+3, bytes.Clone(f.b.PageBytes(2)), dbPages+3)
	_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	if v.Info().SchemaCookie != 77 {
		t.Fatalf("page 1 of the WAL was not adopted: %+v", v.Info())
	}
	if got := v.Info().FileSize; got != int64(len(db)) {
		t.Errorf("FileSize %d, want the database file's %d", got, len(db))
	}
}

// TestLiveSingleGapPageIsReported: one missing page is counted as one.
func TestLiveSingleGapPageIsReported(t *testing.T) {
	f := newWfix(20)
	db := walMode(f.b.Bytes())
	dbPages := f.b.Snapshot().Pages()
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(dbPages+2, bytes.Clone(f.b.PageBytes(2)), dbPages+2)
	_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	x, ok := findWarn(v, sqlitefile.WarnLivePagesUnavailable)
	if !ok || x.File != sqlitefile.FileDB || !strings.Contains(x.Msg, "1 pages") || x.Page != dbPages+1 {
		t.Errorf("%v", v.Warnings())
	}
}
