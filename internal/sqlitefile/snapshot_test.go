package sqlitefile_test

// The as-of sources of page images (plan 3I, Task 10): the state of the writer
// at the time of an image, ordered by WAL generation first and by slot only
// within one generation.

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// snapScenario is a database of 6 pages and a WAL with a current generation
// (slots 1..5) and a stale one (slots 6..9, left behind by a reset):
//
//	current s1 p4, s2 p3 (commit 6), s3 p2, s4 p3 (commit 6), s5 p3
//	stale   s6 p5, s7 p6, s8 p5, s9 p2
type snapScenario struct {
	db, wal []byte
	d       *sqlitefile.DB
	h       *sqlitefile.Hist
	byFrame map[int]sqlitefile.PageImage // WAL images by slot
}

func newSnapScenario(t *testing.T) *snapScenario {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	b.CreateTable("t", "create table t(a)")
	db := b.Bytes()
	for pg := 3; pg <= 6; pg++ {
		db = append(db, patPage(byte(pg))...)
	}
	db = walMode(withCount(db, 6))
	w := b.NewWAL(false, 0x10, 0x11, 0)
	for i, pg := range []uint32{5, 5, 5, 5, 5, 5, 6, 5, 2} { // the old generation
		w.Frame(pg, patPage(byte(0x30+i)), 0)
	}
	w.Reset(0x20, 0x21)
	w.Frame(4, patPage(0x41), 0)
	w.Frame(3, patPage(0x42), 6)
	w.Frame(2, patPage(0x43), 0)
	w.Frame(3, patPage(0x44), 6)
	w.Frame(3, patPage(0x45), 0)
	wal := w.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	s := &snapScenario{db: db, wal: wal, d: d, h: h, byFrame: map[int]sqlitefile.PageImage{}}
	for _, p := range collectPages(t, h) {
		if p.WAL != nil {
			s.byFrame[int(p.WAL.Frame)] = p
		}
	}
	return s
}

func (s *snapScenario) frame(slot int) []byte { return s.wal[walOff(slot) : walOff(slot)+hps] }

func (s *snapScenario) page(pg int) []byte { return s.db[(pg-1)*hps : pg*hps] }

// readAs reads page pg in the state of image img and reports the source: the
// WAL slot (> 0), 0 for the database file, or -1 when it is unavailable.
func readAs(t *testing.T, img sqlitefile.PageImage, pg uint32) (data []byte, slot int) {
	t.Helper()
	data, loc, err := sqlitefile.SnapshotRead(img, pg)
	switch {
	case err == nil && loc.File == sqlitefile.FileWAL:
		return data, int(loc.Frame)
	case err == nil && loc.File == sqlitefile.FileDB:
		return data, 0
	case errors.Is(err, sqlitefile.ErrPageUnavailable):
		return nil, -1
	}
	t.Fatalf("page %d as of %s: %v (loc %+v)", pg, describe(img), err, loc)
	return nil, -2
}

// TestSnapshotRuleByGeneration: the page a frame's chain sees is the latest
// frame of the frame's own generation at a slot not above its own; the database
// page only when the current generation never wrote the page; otherwise it is
// unavailable. Later slots of the generation are never read.
func TestSnapshotRuleByGeneration(t *testing.T) {
	s := newSnapScenario(t)
	// current generation, frame s2 (a superseded frame of page 3)
	img := s.byFrame[2]
	if img.WAL == nil || img.WAL.Generation != 0 {
		t.Fatalf("frame 2: %+v", img.WAL)
	}
	if data, slot := readAs(t, img, 3); slot != 2 || !bytes.Equal(data, s.frame(2)) {
		t.Errorf("page 3 as of s2: slot %d, want the latest at or below it, 2 (not 4 or 5)", slot)
	}
	if data, slot := readAs(t, img, 4); slot != 1 || !bytes.Equal(data, s.frame(1)) {
		t.Errorf("page 4 as of s2: slot %d, want 1", slot)
	}
	if data, slot := readAs(t, img, 6); slot != 0 || !bytes.Equal(data, s.page(6)) {
		t.Errorf("page 6 as of s2: slot %d, want the database page (the generation never wrote it)", slot)
	}
	// page 2 is rewritten by the generation only at s3, after s2: the database page
	// may already have been checkpointed over, so it is unavailable, and the later
	// frame is never used
	if _, slot := readAs(t, img, 2); slot != -1 {
		t.Errorf("page 2 as of s2: slot %d, want unavailable", slot)
	}
	// as of s5 (uncommitted, last of the generation): the latest frames at or below
	if _, slot := readAs(t, s.byFrame[5], 3); slot != 5 {
		t.Errorf("page 3 as of s5: slot %d, want 5", slot)
	}
	if _, slot := readAs(t, s.byFrame[5], 2); slot != 3 {
		t.Errorf("page 2 as of s5: slot %d, want 3", slot)
	}
	// stale generation, frame s9 (page 2): its own earlier frames only
	st := s.byFrame[9]
	if st.WAL == nil || st.WAL.Generation != 1 || st.Origin != sqlitefile.OriginWALStale {
		t.Fatalf("frame 9: %s", describe(st))
	}
	if data, slot := readAs(t, st, 5); slot != 8 || !bytes.Equal(data, s.frame(8)) {
		t.Errorf("page 5 as of stale s9: slot %d, want 8", slot)
	}
	if data, slot := readAs(t, st, 6); slot != 7 || !bytes.Equal(data, s.frame(7)) {
		t.Errorf("page 6 as of stale s9: slot %d, want 7", slot)
	}
	// and as of stale s7 the frame at slot 8 is later, so it is not seen
	if _, slot := readAs(t, s.byFrame[7], 5); slot != 6 {
		t.Errorf("page 5 as of stale s7: slot %d, want 6, not the later 8", slot)
	}
	if !hasCode(s.h.Warnings(), sqlitefile.WarnSnapshotUnavailable) {
		t.Errorf("no snapshot-unavailable warning: %v", s.h.Warnings())
	}
}

// TestSnapshotOrdersByGenerationNotSlot: a stale-generation frame at slot 9 and
// a current-generation frame of page 3 at slot 2: the stale image never reads
// the slot-2 page (slot order is not time order across generations), and the
// database page is no substitute for a stale generation.
func TestSnapshotOrdersByGenerationNotSlot(t *testing.T) {
	s := newSnapScenario(t)
	st := s.byFrame[9]
	for _, pg := range []uint32{3, 4} { // written by the current generation only; the file has both
		if data, slot := readAs(t, st, pg); slot != -1 || data != nil {
			t.Errorf("page %d as of stale s9: slot %d, want unavailable", pg, slot)
		}
	}
	w := s.h.Warnings()
	found := 0
	for _, x := range w {
		if x.Code == sqlitefile.WarnSnapshotUnavailable && (x.Page == 3 || x.Page == 4) {
			found++
		}
	}
	if found != 2 {
		t.Errorf("%d snapshot-unavailable warnings for pages 3 and 4: %v", found, w)
	}
	// the same pages as of a current-generation frame at a later slot are served
	if _, slot := readAs(t, s.byFrame[5], 4); slot != 1 {
		t.Errorf("page 4 as of s5: slot %d", slot)
	}
}

// TestSnapshotDBUnderWALAndRolledBack: the database image under the WAL sees only
// database pages with no committed overlay; the rolled-back image sees the
// database file as found whatever the WAL or the journal say; a journal
// before-image sees the rollback state with the WAL ignored.
func TestSnapshotDBUnderWALAndRolledBack(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	imgs := collectPages(t, h)
	find := func(o sqlitefile.Origin, pg uint32, rec int) sqlitefile.PageImage {
		for _, p := range imgs {
			if p.Origin == o && p.Number == pg && (rec < 0 || (p.Journal != nil && p.Journal.Record == rec)) {
				return p
			}
		}
		t.Fatalf("no image %d/%d/%d", o, pg, rec)
		return sqlitefile.PageImage{}
	}
	dbp := func(pg int) []byte { return m.db[(pg-1)*hps : pg*hps] }
	recs := d.Journal().Records
	jdata := func(i int) []byte { return m.journal[recs[i].Offset : recs[i].Offset+hps] }
	check := func(what string, img sqlitefile.PageImage, pg uint32, want []byte, wantFile sqlitefile.FileKind, wantRec int) {
		t.Helper()
		data, loc, err := sqlitefile.SnapshotRead(img, pg)
		if want == nil {
			if !errors.Is(err, sqlitefile.ErrPageUnavailable) {
				t.Errorf("%s: page %d: %v, want unavailable", what, pg, err)
			}
			return
		}
		if err != nil || !bytes.Equal(data, want) || loc.File != wantFile || (wantFile == sqlitefile.FileJournal && loc.Record != wantRec) {
			t.Errorf("%s: page %d: loc %+v err %v", what, pg, loc, err)
		}
	}
	under := find(sqlitefile.OriginDBUnderWAL, 3, -1)
	check("under-wal", under, 2, nil, 0, 0)                    // overlaid by a committed frame
	check("under-wal", under, 4, dbp(4), sqlitefile.FileDB, 0) // only an uncommitted frame
	check("under-wal", under, 8, dbp(8), sqlitefile.FileDB, 0) // no frame at all
	check("under-wal", under, 7, dbp(7), sqlitefile.FileDB, 0) // the journal is not consulted
	rolled := find(sqlitefile.OriginDBRolledBack, 7, -1)
	check("rolled-back", rolled, 7, dbp(7), sqlitefile.FileDB, 0)
	check("rolled-back", rolled, 3, dbp(3), sqlitefile.FileDB, 0) // the WAL is ignored
	check("rolled-back", rolled, 14, dbp(14), sqlitefile.FileDB, 0)
	jb0 := find(sqlitefile.OriginJournalBefore, 7, 0) // a record that is not the winner
	check("journal rec0", jb0, 7, jdata(2), sqlitefile.FileJournal, 2)
	jb2 := find(sqlitefile.OriginJournalBefore, 9, 1)
	check("journal rec1", jb2, 9, jdata(1), sqlitefile.FileJournal, 1)
	check("journal rec1", jb2, 8, dbp(8), sqlitefile.FileDB, 0) // its record has a bad checksum: not played
	check("journal rec1", jb2, 3, dbp(3), sqlitefile.FileDB, 0) // the WAL is ignored
	check("journal rec1", jb2, 14, nil, 0, 0)                   // above the journal's initial size
	check("journal rec1", jb2, 6, dbp(6), sqlitefile.FileDB, 0) // rec4 follows the bad record
	// a freelist leaf follows only free pages and orphans of the live state
	leaf := find(sqlitefile.OriginFreelistLeaf, 6, -1)
	check("freelist leaf", leaf, 7, jdata(2), sqlitefile.FileJournal, 2) // another leaf, as Live shows it
	check("freelist leaf", leaf, 10, dbp(10), sqlitefile.FileDB, 0)      // an orphan
	check("freelist leaf", leaf, 3, nil, 0, 0)                           // a live b-tree page
	check("freelist leaf", leaf, 5, nil, 0, 0)                           // the trunk
	check("freelist leaf", leaf, 20, nil, 0, 0)                          // not in the file
}

// chainScenario is a table whose row 1 has an overflow chain (pages 3 and 4) and
// a WAL whose old generation holds other versions of the leaf and its chain.
type chainScenario struct {
	db, wal  []byte
	c3, c4   []byte // the old generation's chain pages
	leafSlot int
	d        *sqlitefile.DB
	h        *sqlitefile.Hist
}

func newChainScenario(t *testing.T, withSecond bool) *chainScenario {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a)")
	tb.Insert(1, bytes.Repeat([]byte("blob"), 625)) // 2500 bytes
	chain := tb.Overflow(1)
	if len(chain) != 2 || chain[0] != 3 || chain[1] != 4 {
		t.Fatalf("overflow pages %v", chain)
	}
	db := walMode(b.Bytes())
	leaf := b.PageBytes(2)
	// the old generation's versions of the chain pages: same pointers, new content
	c3 := patPage(0x71)
	copy(c3[:4], b.PageBytes(3)[:4])
	c4 := patPage(0x72)
	copy(c4[:4], b.PageBytes(4)[:4])
	w := b.NewWAL(false, 0x10, 0x11, 0)
	w.Frame(5, patPage(0x01), 0) // overwritten by the current generation
	w.Frame(3, c3, 0)
	if withSecond {
		w.Frame(4, c4, 0)
	}
	w.Frame(2, leaf, 0)
	slot := 3
	if withSecond {
		slot = 4
	}
	w.Reset(0x20, 0x21)
	w.Frame(5, patPage(0x02), 0)
	wal := w.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	return &chainScenario{db: db, wal: wal, c3: c3, c4: c4, leafSlot: slot, d: d, h: h}
}

func (c *chainScenario) leafImage(t *testing.T) sqlitefile.PageImage {
	t.Helper()
	for _, p := range collectPages(t, c.h) {
		if p.WAL != nil && int(p.WAL.Frame) == c.leafSlot {
			return p
		}
	}
	t.Fatal("no leaf image")
	return sqlitefile.PageImage{}
}

// TestSnapshotOverflowChain: a cell of a stale frame's page follows its overflow
// chain through the stale generation's own frames: the bytes are the old
// versions, not the database's; a chain page the generation never wrote ends the
// chain as snapshot-unavailable and the value is truncated, never filled in from
// the database or from a newer generation.
func TestSnapshotOverflowChain(t *testing.T) {
	t.Run("whole chain in the stale generation", func(t *testing.T) {
		c := newChainScenario(t, true)
		img := c.leafImage(t)
		pp, err := img.Parse()
		if err != nil || len(pp.Cells) != 1 {
			t.Fatalf("parse: %v %+v", err, pp)
		}
		cell := pp.Cells[0]
		got, damage, err := sqlitefile.SnapshotPayload(img, cell)
		if err != nil || damage != "" {
			t.Fatalf("payload: %v %q", err, damage)
		}
		want := append(append(bytes.Clone(cell.LocalBytes), c.c3[4:]...), c.c4[4:]...)[:cell.PayloadLen]
		if !bytes.Equal(got, want) {
			t.Errorf("the payload is not the stale generation's version (%d bytes)", len(got))
		}
	})
	t.Run("chain page the generation never wrote", func(t *testing.T) {
		c := newChainScenario(t, false)
		img := c.leafImage(t)
		pp, err := img.Parse()
		if err != nil || len(pp.Cells) != 1 {
			t.Fatalf("parse: %v %+v", err, pp)
		}
		cell := pp.Cells[0]
		got, damage, err := sqlitefile.SnapshotPayload(img, cell)
		if err != nil {
			t.Fatal(err)
		}
		want := append(bytes.Clone(cell.LocalBytes), c.c3[4:]...)
		if damage == "" || !bytes.Equal(got, want) {
			t.Errorf("%d bytes, damage %q; want the %d bytes up to page 4", len(got), damage, len(want))
		}
		found := false
		for _, x := range c.h.Warnings() {
			found = found || (x.Code == sqlitefile.WarnSnapshotUnavailable && x.Page == 4)
		}
		if !found {
			t.Errorf("no snapshot-unavailable warning for page 4: %v", c.h.Warnings())
		}
	})
}
