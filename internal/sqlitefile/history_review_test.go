package sqlitefile_test

// Task 10 review fixes (step 0b): the partial page marker, the listing-once
// rule, the commit-size boundary, the WAL guard, every journalState branch,
// reserved bytes in spans and the beyond-end chain.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// reviewBuilder returns a builder of a 4-page database (schema, table root 2,
// leaves 3 and 4) and its page snapshots.
func reviewBuilder(t testing.TB) (*sqlitetest.Builder, *sqlitetest.Image, []byte) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 12; id++ {
		tb.Insert(id, id*3, longText(100, byte(id)))
	}
	db := b.Bytes()
	if len(db) != 4*hps {
		t.Fatalf("%d pages, want 4", len(db)/hps)
	}
	return b, b.Snapshot(), db
}

type originPage struct {
	o  sqlitefile.Origin
	pg uint32
}

func nonLive(imgs []sqlitefile.PageImage) []originPage {
	var out []originPage
	for _, p := range imgs {
		if p.Origin != sqlitefile.OriginLiveBTree {
			out = append(out, originPage{p.Origin, p.Number})
		}
	}
	return out
}

// TestPartialTrailingPageIsMarked: a trailing page of 300 bytes is listed with
// its short Bytes, marked Partial with a Note; whole pages are not.
func TestPartialTrailingPageIsMarked(t *testing.T) {
	_, _, db := reviewBuilder(t)
	db = withCount(db, 4)
	db = append(db, patPage(0x55)[:hps]...) // page 5, whole
	db = append(db, patPage(0x66)[:300]...) // page 6, partial
	_, h := historyOf(t, db, sqlitefile.Options{})
	var whole, part *sqlitefile.PageImage
	for _, p := range collectPages(t, h) {
		switch {
		case p.Origin == sqlitefile.OriginBeyondEnd && p.Number == 5:
			whole = &p
		case p.Origin == sqlitefile.OriginBeyondEnd && p.Number == 6:
			part = &p
		}
	}
	if whole == nil || part == nil {
		t.Fatalf("beyond-end images: whole %v partial %v", whole, part)
	}
	if whole.Partial || whole.Note != "" {
		t.Errorf("a whole page is marked: %+v", *whole)
	}
	b, err := part.Bytes()
	if err != nil || len(b) != 300 {
		t.Fatalf("Bytes: %d bytes, %v", len(b), err)
	}
	if !part.Partial || part.Note == "" {
		t.Errorf("a 300-byte page is not marked partial: Partial %v Note %q", part.Partial, part.Note)
	}
}

// reviewMaster: a database of 4 pages plus a whole page 5 beyond the header
// count, a WAL (s1 p4 uncommitted-in-group, s2 p3, s3 p5 junk, s4 p4 commit with
// size 4) and a hot journal (initial size 4, records for pages 2 and 3, so page
// 3 is both journal-hidden and WAL-overlaid).
func reviewMaster(t testing.TB) *histMaster {
	t.Helper()
	b, snap, db := reviewBuilder(t)
	db = walMode(withCount(append(db, patPage(0x55)...), 4))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(4, patPage(0x41), 0)
	w.Frame(3, snap.Page(3), 0)
	w.Frame(5, patPage(0x43), 0)
	w.Frame(4, snap.Page(4), 4)
	j := b.NewJournal(512, 0xfeedbeef, 4)
	j.Record(2, snap.Page(2))
	j.Record(3, snap.Page(3))
	return &histMaster{db: db, wal: w.Bytes(), journal: j.Bytes()}
}

// TestHistoryListingOnceAndCommitBoundary pins, on one scenario: a page that is
// both journal-hidden and WAL-overlaid is listed once, as rolled back; a page
// equal to the commit's page count is an ordinary page (its latest frame is the
// live one, its older frame is superseded without a note) while a page above it
// is flagged; and a database page above the count is never under the WAL.
func TestHistoryListingOnceAndCommitBoundary(t *testing.T) {
	m := reviewMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	imgs := collectPages(t, h)
	want := []originPage{
		{sqlitefile.OriginDBUnderWAL, 4},
		{sqlitefile.OriginDBRolledBack, 2},
		{sqlitefile.OriginDBRolledBack, 3},
		{sqlitefile.OriginDBRolledBack, 5},
		{sqlitefile.OriginWALSuperseded, 4},
		{sqlitefile.OriginWALSuperseded, 5},
		{sqlitefile.OriginJournalBefore, 2},
		{sqlitefile.OriginJournalBefore, 3},
	}
	if got := nonLive(imgs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history\n got  %v\n want %v", got, want)
	}
	for _, p := range imgs {
		if p.Origin == sqlitefile.OriginWALSuperseded {
			wantNote := ""
			if p.Number == 5 {
				wantNote = "page-beyond-commit-size"
			}
			if p.WAL.Note != wantNote {
				t.Errorf("superseded page %d slot %d: note %q, want %q", p.Number, p.WAL.Frame, p.WAL.Note, wantNote)
			}
		}
		if p.Origin == sqlitefile.OriginDBRolledBack && p.Number == 5 && (p.Journal == nil || p.Journal.Note != sqlitefile.JournalNoteBeyondInitialSize) {
			t.Errorf("page 5 is above the journal's initial size: %+v", p.Journal)
		}
	}
	// the live page 4 comes from the commit frame (slot 4), page 3 from slot 2
	for _, p := range imgs {
		if p.Origin == sqlitefile.OriginLiveBTree && p.Number == 4 && p.Loc.Frame != 4 {
			t.Errorf("live page 4 is at %+v, want slot 4", p.Loc)
		}
	}
}

// TestSnapshotUnderWALIgnoresFramesBeyondTheCommitSize: a committed frame above
// the commit's page count does not overlay anything, so the database page past
// the count stays readable in the image of the database under the WAL.
func TestSnapshotUnderWALIgnoresFramesBeyondTheCommitSize(t *testing.T) {
	m := reviewMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	under := imageOf(t, h, sqlitefile.OriginDBUnderWAL, 4)
	if data, _, err := sqlitefile.SnapshotRead(under, 5); err != nil || !bytes.Equal(data, patPage(0x55)) {
		t.Errorf("page 5 under the WAL: %v", err)
	}
	if _, _, err := sqlitefile.SnapshotRead(under, 3); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("page 3 is overlaid by a committed frame: %v", err)
	}
	if _, _, err := sqlitefile.SnapshotRead(under, 4); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("page 4 is overlaid by a committed frame: %v", err)
	}
}

// TestHistoryIgnoresAWALTheLiveViewDoesNotUse: a WAL whose header is invalid
// overlays nothing, so no database page is listed under it.
func TestHistoryIgnoresAWALTheLiveViewDoesNotUse(t *testing.T) {
	m := reviewMaster(t)
	m.wal[0] ^= 0xff
	d := m.open(t, sqlitefile.Options{}, true)
	if info := d.WAL().Info; info.UsedByLive {
		t.Fatalf("the WAL is used: %+v", info)
	}
	h := d.History()
	t.Cleanup(h.Release)
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginDBUnderWAL || (p.WAL != nil && p.WAL.State != sqlitefile.FrameUnanchored) {
			t.Errorf("image of an unused WAL that is not an unverified frame: %s", describe(p))
		}
	}
}

// TestSnapshotBeyondEndChainFollowsPagesPastTheLiveCount: the image of a page
// past the live count may follow a chain into the database pages past it, no
// other image may, and the live pages are not followed.
func TestSnapshotBeyondEndChainFollowsPagesPastTheLiveCount(t *testing.T) {
	_, _, db := reviewBuilder(t)
	db = withCount(db, 4)
	db = append(db, patPage(0x55)...)
	db = append(db, patPage(0x56)...)
	_, h := historyOf(t, db, sqlitefile.Options{})
	be := imageOf(t, h, sqlitefile.OriginBeyondEnd, 5)
	if data, _, err := sqlitefile.SnapshotRead(be, 6); err != nil || !bytes.Equal(data, patPage(0x56)) {
		t.Errorf("beyond-end image reads page 6: %v", err)
	}
	if _, _, err := sqlitefile.SnapshotRead(be, 3); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("beyond-end image reads live page 3: %v", err)
	}
	// an orphan or freelist image never follows into the pages past the count
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	b2.CreateTable("t", "create table t(a)")
	b2.SetFreelist([][]uint32{{3, 4}})
	db2 := append(b2.Bytes(), patPage(0x57)...)
	db2 = withCount(db2, uint32(len(db2)/hps)-1)
	_, h2 := historyOf(t, db2, sqlitefile.Options{})
	leaf := imageOf(t, h2, sqlitefile.OriginFreelistLeaf, 4)
	if _, _, err := sqlitefile.SnapshotRead(leaf, uint32(len(db2)/hps)); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("freelist image reads a page past the count: %v", err)
	}
}

// ---- journalState: every branch ----

// reviewJournalDB is the 4-page database with a table, in rollback mode.
func reviewJournal(t testing.TB, build func(j *sqlitetest.Journal, snap *sqlitetest.Image)) (db, journal []byte) {
	t.Helper()
	b, snap, db := reviewBuilder(t)
	db = withCount(db, 4)
	j := b.NewJournal(512, 0xfeedbeef, 4)
	build(j, snap)
	return db, j.Bytes()
}

func journalHistory(t testing.TB, db, journal []byte) (*sqlitefile.DB, *sqlitefile.Hist) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	return d, h
}

func recLoc(t testing.TB, d *sqlitefile.DB, i int) int64 {
	t.Helper()
	return d.Journal().Records[i].Offset
}

// TestSnapshotOfAJournalThatIsNotApplied: a hot journal that names a super-journal
// the case does not hold is not applied (the engine skips it), so Live is the
// database as found; its before-images are still listed and the state they
// describe is computed with the playback rules: in order, a bad checksum or page
// 0 ends it, a page above the initial size is skipped, the last write to a page
// wins, the initial size itself is allowed.
func TestSnapshotOfAJournalThatIsNotApplied(t *testing.T) {
	pat := func(s byte) []byte { return patPage(s) }
	db, journal := reviewJournal(t, func(j *sqlitetest.Journal, _ *sqlitetest.Image) {
		j.Record(2, pat(0xa0)) // r0
		j.Record(3, pat(0xa1)) // r1
		j.Record(3, pat(0xa2)) // r2: the last write to page 3 wins
		j.Record(4, pat(0xa3)) // r3: page 4 is the initial size: allowed
		j.Record(9, pat(0xa4)) // r4: above the initial size: skipped
		j.Record(0, pat(0xa5)) // r5: page 0 ends playback
		j.Record(2, pat(0xa6)) // r6: never played
		j.SuperJournal("super-journal-name")
	})
	d, h := journalHistory(t, db, journal)
	info := d.Journal().Info
	if info.Applied || !info.Hot || info.NotAppliedReason != "super-journal-unknown" {
		t.Fatalf("journal %+v", info)
	}
	if !hasCode(h.Warnings(), sqlitefile.WarnJournalSuperUnknown) || !hasCode(h.Warnings(), sqlitefile.WarnJournalHot) {
		t.Errorf("journal warnings missing: %v", h.Warnings())
	}
	var anyBefore sqlitefile.PageImage
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginDBRolledBack {
			t.Errorf("a journal that is not applied hides nothing: %s", describe(p))
		}
		if p.Origin == sqlitefile.OriginJournalBefore {
			anyBefore = p
		}
	}
	if anyBefore.Origin == 0 {
		t.Fatal("no journal before-image listed")
	}
	at := func(pg uint32) (sqlitefile.FileKind, int64, error) {
		_, loc, err := sqlitefile.SnapshotRead(anyBefore, pg)
		return loc.File, loc.Offset, err
	}
	for _, c := range []struct {
		pg  uint32
		rec int // the record that supplies it; -1 the database file
	}{{1, -1}, {2, 0}, {3, 2}, {4, 3}} {
		file, off, err := at(c.pg)
		wantFile, wantOff := sqlitefile.FileDB, int64(c.pg-1)*hps
		if c.rec >= 0 {
			wantFile, wantOff = sqlitefile.FileJournal, recLoc(t, d, c.rec)
		}
		if err != nil || file != wantFile || off != wantOff {
			t.Errorf("page %d: file %v offset %d err %v, want %v offset %d", c.pg, file, off, err, wantFile, wantOff)
		}
	}
	if _, _, err := at(9); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("page 9 is above the initial size: %v", err)
	}
}

// TestSnapshotJournalPlaybackEndsAtABadChecksum: a record that fails its checksum
// ends the state; later records are not played.
func TestSnapshotJournalPlaybackEndsAtABadChecksum(t *testing.T) {
	db, journal := reviewJournal(t, func(j *sqlitetest.Journal, _ *sqlitetest.Image) {
		j.Record(2, patPage(0xb0))
		j.RawRecord(3, patPage(0xb1), 1) // bad checksum
		j.Record(4, patPage(0xb2))
		j.SuperJournal("super-journal-name")
	})
	d, h := journalHistory(t, db, journal)
	if d.Journal().Info.Applied {
		t.Fatal("applied")
	}
	var img sqlitefile.PageImage
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginJournalBefore {
			img = p
		}
	}
	for _, c := range []struct {
		pg  uint32
		rec int
	}{{2, 0}, {3, -1}, {4, -1}} {
		_, loc, err := sqlitefile.SnapshotRead(img, c.pg)
		wantFile, wantOff := sqlitefile.FileDB, int64(c.pg-1)*hps
		if c.rec >= 0 {
			wantFile, wantOff = sqlitefile.FileJournal, recLoc(t, d, c.rec)
		}
		if err != nil || loc.File != wantFile || loc.Offset != wantOff {
			t.Errorf("page %d: %+v %v, want %v at %d", c.pg, loc, err, wantFile, wantOff)
		}
	}
}

// TestSnapshotFollowsTheDatabaseWhenTheJournalIsNotHotOrInvalid: a journal that
// is not hot, or whose header is invalid, is not applied and lists no
// before-image; the history is the database as found, with the journal's own
// warnings.
func TestSnapshotFollowsTheDatabaseWhenTheJournalIsNotHotOrInvalid(t *testing.T) {
	good := func(j *sqlitetest.Journal, _ *sqlitetest.Image) { j.Record(2, patPage(0xc0)) }
	for _, c := range []struct {
		name   string
		mutate func(j []byte)
		reason string
		warn   string
	}{
		{"not hot", func(j []byte) { j[0] = 0 }, "not-hot", ""},
		{"header invalid", func(j []byte) { j[3] ^= 0xff }, "header-invalid", sqlitefile.WarnJournalHeaderInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, journal := reviewJournal(t, good)
			c.mutate(journal)
			d, h := journalHistory(t, db, journal)
			if in := d.Journal().Info; in.Applied || in.NotAppliedReason != c.reason {
				t.Fatalf("journal %+v", in)
			}
			for _, p := range collectPages(t, h) {
				if p.Origin == sqlitefile.OriginJournalBefore || p.Origin == sqlitefile.OriginDBRolledBack {
					t.Errorf("image of a journal that is not applied: %s", describe(p))
				}
			}
			if c.warn != "" && !hasCode(h.Warnings(), c.warn) {
				t.Errorf("no %s warning: %v", c.warn, h.Warnings())
			}
			if hasCode(h.Warnings(), sqlitefile.WarnJournalHot) && c.reason == "not-hot" {
				t.Errorf("journal-hot warned for a journal that is not hot")
			}
			// live pages follow the database
			live := imageOf(t, h, sqlitefile.OriginLiveBTree, 2)
			if data, err := live.Bytes(); err != nil || !bytes.Equal(data, db[hps:2*hps]) {
				t.Errorf("live page 2 is not the database's: %v", err)
			}
		})
	}
}

// ---- spans with reserved bytes ----

// TestSpansStopAtTheUsableArea: with reserved bytes at the end of every page the
// gap and the freeblocks end at the usable size; a content start or a freeblock
// that reaches into the reserved area is clipped or rejected.
func TestSpansStopAtTheUsableArea(t *testing.T) {
	const reserved = 32
	b := sqlitetest.New(sqlitetest.Options{PageSize: sps, Reserved: reserved})
	b.CreateTable("t", "create table t(a)")
	db := b.Bytes()
	usable := sps - reserved
	page := func(set func(p []byte)) []byte {
		p := make([]byte, sps)
		p[0] = 0x0d
		set(p)
		return p
	}
	// content start at the page size (0 cells): the gap ends at the usable size
	gapPage := page(func(p []byte) { binary.BigEndian.PutUint16(p[5:], uint16(sps)) })
	// a freeblock whose end lies in the reserved area
	fbPage := page(func(p []byte) {
		binary.BigEndian.PutUint16(p[5:], 100)
		setFirst(p, usable-8)
		fb(p, usable-8, 0, 16)
	})
	db = append(db, gapPage...)
	db = append(db, fbPage...)
	db = withCount(db, uint32(len(db)/sps))
	_, h := historyOf(t, db, sqlitefile.Options{})
	pp, err := imageOf(t, h, sqlitefile.OriginOrphan, 3).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := spanList(pp.Spans), [][3]int{{int(sqlitefile.SpanGap), 8, usable - 8}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("gap page spans %v, want %v", got, want)
	}
	pp, err = imageOf(t, h, sqlitefile.OriginOrphan, 4).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if pp.Rejected != 1 || !hasCode(h.Warnings(), sqlitefile.WarnFreeblockChain) {
		t.Errorf("a freeblock into the reserved area: rejected %d, spans %v", pp.Rejected, spanList(pp.Spans))
	}
	for _, s := range pp.Spans {
		if s.Offset+s.Length > usable {
			t.Errorf("span %+v lies in the reserved area", s)
		}
	}
}

// ---- exact span rules (the freeblock chain, the gap, the trunk tail) ----

func TestFreeblockChainExactBoundaries(t *testing.T) {
	// a block that starts exactly at the end of the one before it is accepted;
	// one byte earlier overlaps and is rejected
	adjacent := leaf512(func(p []byte) { setFirst(p, 412); fb(p, 412, 440, 28); fb(p, 440, 0, 4) })
	overlap := leaf512(func(p []byte) { setFirst(p, 412); fb(p, 412, 439, 28); fb(p, 439, 0, 4) })
	for _, c := range []struct {
		name     string
		page     []byte
		kept     int
		rejected int
	}{{"adjacent", adjacent, 2, 0}, {"overlap by one byte", overlap, 1, 1}} {
		t.Run(c.name, func(t *testing.T) {
			_, h := historyOf(t, pagesDB(t, c.page), sqlitefile.Options{})
			pp, err := imageOf(t, h, sqlitefile.OriginOrphan, 3).Parse()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, s := range pp.Spans {
				if s.Kind == sqlitefile.SpanFreeblock {
					n++
				}
			}
			if n != c.kept || pp.Rejected != c.rejected {
				t.Errorf("kept %d (want %d) rejected %d (want %d)", n, c.kept, pp.Rejected, c.rejected)
			}
		})
	}
}

// TestNoZeroLengthSpans: a pointer array that reaches the content start leaves
// no gap span, and a trunk whose leaf list ends exactly at the usable size has no
// tail span; a freelist leaf never has a span.
func TestNoZeroLengthSpans(t *testing.T) {
	// 3 cells, array to 14; content start 14: the gap is empty
	full := leaf512(func(p []byte) { binary.BigEndian.PutUint16(p[5:], 14) })
	_, h := historyOf(t, pagesDB(t, full), sqlitefile.Options{})
	pp, err := imageOf(t, h, sqlitefile.OriginOrphan, 3).Parse()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range pp.Spans {
		if s.Length <= 0 {
			t.Errorf("zero-length span %+v", s)
		}
	}
	if len(pp.Spans) != 0 {
		t.Errorf("spans %v, want none", spanList(pp.Spans))
	}

	// a trunk of 126 leaves on a 512-byte page: 8 + 4*126 = 504 = the usable size
	// minus 8 ... use a count that ends exactly at usable
	for _, reserved := range []int{0, 8} {
		b := sqlitetest.New(sqlitetest.Options{PageSize: sps, Reserved: reserved})
		b.CreateTable("t", "create table t(a)")
		db := b.Bytes()
		leaves := (sps - reserved - 8) / 4
		trunk := make([]byte, sps)
		binary.BigEndian.PutUint32(trunk[4:], uint32(leaves))
		for i := range leaves {
			binary.BigEndian.PutUint32(trunk[8+4*i:], uint32(4+i))
		}
		db = append(db, trunk...)
		for range leaves {
			db = append(db, make([]byte, sps)...)
		}
		db = withCount(db, uint32(len(db)/sps))
		binary.BigEndian.PutUint32(db[32:], 3)
		binary.BigEndian.PutUint32(db[36:], uint32(leaves+1))
		_, h := historyOf(t, db, sqlitefile.Options{})
		if tr := imageOf(t, h, sqlitefile.OriginFreelistTrunk, 3); len(tr.Spans) != 0 {
			t.Errorf("reserved %d: trunk spans %v, want none", reserved, tr.Spans)
		}
		if lf := imageOf(t, h, sqlitefile.OriginFreelistLeaf, 4); len(lf.Spans) != 0 {
			t.Errorf("reserved %d: leaf spans %v", reserved, lf.Spans)
		}
	}
}

// TestFreelistLeafBytesAreNotSpans: a freelist leaf that still holds a b-tree
// page image has no span; its bytes are stale content, not free space.
func TestFreelistLeafBytesAreNotSpans(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: sps})
	b.CreateTable("t", "create table t(a)")
	b.SetFreelist([][]uint32{{3, 4}})
	db := b.Bytes()
	copy(db[3*sps:], leaf512(func(p []byte) { setFirst(p, 412); fb(p, 412, 0, 28) }))
	_, h := historyOf(t, db, sqlitefile.Options{})
	if lf := imageOf(t, h, sqlitefile.OriginFreelistLeaf, 4); len(lf.Spans) != 0 {
		t.Errorf("leaf spans %v", lf.Spans)
	}
}

// TestFramesOfOnePageKeepSlotOrder: many frames of one page in one generation
// are listed in slot order.
func TestFramesOfOnePageKeepSlotOrder(t *testing.T) {
	b, snap, db := reviewBuilder(t)
	db = walMode(withCount(db, 4))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	const n = 60
	for i := range n {
		w.Frame(3, patPage(byte(i)), 0)
	}
	w.Frame(3, snap.Page(3), 4)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wal := w.Bytes()
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	last := uint32(0)
	count := 0
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginWALSuperseded {
			count++
			if p.WAL.Frame <= last {
				t.Fatalf("slot %d after slot %d", p.WAL.Frame, last)
			}
			last = p.WAL.Frame
		}
	}
	if count != n {
		t.Errorf("%d superseded frames, want %d", count, n)
	}
}
