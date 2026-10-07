package sqlitefile_test

// History page images (plan 3I, Task 10): one scenario that holds every origin,
// and the properties every image must have.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const hps = 1024

// patPage is a page whose bytes are never all equal and never all zero.
func patPage(seed byte) []byte {
	p := make([]byte, hps)
	for i := range p {
		p[i] = byte(i*31) + seed
	}
	return p
}

// withCount sets the database size in the header to n pages, trusted.
func withCount(db []byte, n uint32) []byte {
	d := bytes.Clone(db)
	binary.BigEndian.PutUint32(d[28:], n)
	copy(d[92:96], d[24:28])
	return d
}

// histMaster holds every origin: see newHistMaster.
type histMaster struct {
	db, wal, journal []byte
}

func walOff(slot int) int64 { return 32 + int64(slot-1)*(24+hps) + 24 }

// newHistMaster builds the master scenario (page size 1024):
//
//	db (header count 11, 15 pages in the file):
//	  1 schema, 2 root of t (interior), 3 and 4 leaves of t,
//	  5 freelist trunk with leaves 6 (all zero), 7, 8, 9,
//	  10, 11 orphans, 12, 13 beyond the count, 14, 15 beyond the journal's size.
//	journal (hot, initial size 13, nonce 0xdeadbeef):
//	  rec0 p7 valid (a later record for p7 wins), rec1 p9 valid, rec2 p7 valid,
//	  rec3 p8 bad checksum (playback stops), rec4 p6 valid but never played.
//	wal (header salts 0x2000/0x2001, commit size 11):
//	  s1 p3 commit, s2 p20 junk, s3 p3 commit (latest of 3), s4 p2 commit (latest
//	  of 2), s5 p4 uncommitted, s6 p3 broken, s7 p2 detached, s8 p4 and s9 p2 stale.
func newHistMaster(t testing.TB) *histMaster {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 12; id++ {
		tb.Insert(id, id*3, longText(100, byte(id)))
	}
	if got := tb.Pages(); !slices.Equal(got, []uint32{2, 3, 4}) {
		t.Fatalf("the table's pages are %v, the scenario needs 2 3 4", got)
	}
	b.SetFreelist([][]uint32{{5, 6, 7, 8, 9}})
	db := b.Bytes()
	if len(db) != 9*hps {
		t.Fatalf("%d pages", len(db)/hps)
	}
	// page 6: a secure-deleted leaf; 7..9 keep stale bytes; 10..15 are raw
	clear(db[5*hps : 6*hps])
	for pg := 7; pg <= 15; pg++ {
		if pg <= 9 {
			copy(db[(pg-1)*hps+16:pg*hps], patPage(byte(pg))[16:])
		} else {
			db = append(db, patPage(byte(pg))...)
		}
	}
	db = walMode(withCount(db, 11))

	// versions of the leaf holding row 1
	_, leaf, _ := tb.CellBytes(1)
	tb.Update(1, int64(3), longText(100, 91))
	s1 := b.Snapshot()
	tb.Update(1, int64(3), longText(100, 92))
	s2 := b.Snapshot()
	if leaf != 3 {
		t.Fatalf("row 1 lies on page %d", leaf)
	}

	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	for i := 1; i <= 9; i++ { // the generation that will be left behind
		w.Frame(uint32(2+i%3), patPage(byte(0x60+i)), 0)
	}
	w.Reset(0x2000, 0x2001)
	w.Frame(leaf, s1.Page(leaf), 11) // s1
	w.Frame(20, patPage(0xb2), 0)    // s2: beyond the commit size
	w.Frame(leaf, s2.Page(leaf), 11) // s3
	w.Frame(2, s2.Page(2), 11)       // s4
	w.Frame(4, patPage(0xb5), 0)     // s5 uncommitted
	w.Frame(3, patPage(0xb6), 0)     // s6 broken below
	w.Frame(2, patPage(0xb7), 0)     // s7 detached
	wb := w.Bytes()
	w.PatchFrame(6, 24+30, wb[walOff(6)+30]^0xff)
	wal := w.Bytes()
	// the stale generation's slots 8 and 9 are what the old frames 8 and 9 were
	if want := 32 + 9*(24+hps); len(wal) != want {
		t.Fatalf("wal is %d bytes, want %d", len(wal), want)
	}

	j := b.NewJournal(512, 0xdeadbeef, 13)
	j.Record(7, patPage(0xa0))
	j.Record(9, patPage(0xa1))
	j.Record(7, patPage(0xa2))
	j.RawRecord(8, patPage(0xa3), 1)
	j.Record(6, patPage(0xa4))
	return &histMaster{db: db, wal: wal, journal: j.Bytes()}
}

func (m *histMaster) open(t testing.TB, o sqlitefile.Options, walFirst bool) *sqlitefile.DB {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(m.db), int64(len(m.db)), o)
	if err != nil {
		t.Fatal(err)
	}
	aw := func() {
		if _, err := d.AttachWAL(bytes.NewReader(m.wal), int64(len(m.wal))); err != nil {
			t.Fatal(err)
		}
	}
	aj := func() {
		if _, err := d.AttachJournal(bytes.NewReader(m.journal), int64(len(m.journal))); err != nil {
			t.Fatal(err)
		}
	}
	if walFirst {
		aw()
		aj()
	} else {
		aj()
		aw()
	}
	return d
}

func collectPages(t testing.TB, h *sqlitefile.Hist) []sqlitefile.PageImage {
	t.Helper()
	var out []sqlitefile.PageImage
	if err := h.Pages(context.Background(), func(p sqlitefile.PageImage) bool {
		out = append(out, p)
		return true
	}); err != nil {
		t.Fatalf("Pages: %v", err)
	}
	return out
}

// histWant is what one page image must say (spans are checked elsewhere).
type histWant struct {
	origin  sqlitefile.Origin
	num     uint32
	loc     sqlitefile.PageLoc
	wal     *sqlitefile.WALProv
	journal *sqlitefile.JournalProv
}

func dbLoc(pg uint32) sqlitefile.PageLoc {
	return sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: int64(pg-1) * hps}
}

func walLoc(slot int) sqlitefile.PageLoc {
	return sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: walOff(slot), Frame: uint32(slot)}
}

func (m *histMaster) expected(t testing.TB, d *sqlitefile.DB) []histWant {
	t.Helper()
	recs := d.Journal().Records
	if len(recs) != 5 || recs[3].ChecksumOK || !recs[2].ChecksumOK {
		t.Fatalf("journal records %+v", recs)
	}
	jloc := func(i int) sqlitefile.PageLoc {
		return sqlitefile.PageLoc{File: sqlitefile.FileJournal, Offset: recs[i].Offset, Record: i}
	}
	jp := func(i int, applied bool) *sqlitefile.JournalProv {
		return &sqlitefile.JournalProv{Record: i, ChecksumOK: recs[i].ChecksumOK, Hot: true, Applied: applied, Nonce: 0xdeadbeef}
	}
	rb := func(i int) *sqlitefile.JournalProv { return jp(i, true) }
	wp := func(slot int, st sqlitefile.FrameState, gen int, note string) *sqlitefile.WALProv {
		f := d.WAL().Frames[slot-1]
		return &sqlitefile.WALProv{Frame: uint32(slot), Salt1: f.Salt1, Salt2: f.Salt2, State: st, Committed: st == sqlitefile.FrameCommitted, Linked: f.Linked, Generation: gen, Note: note}
	}
	const (
		live  = sqlitefile.OriginLiveBTree
		leaf  = sqlitefile.OriginFreelistLeaf
		trunk = sqlitefile.OriginFreelistTrunk
		orph  = sqlitefile.OriginOrphan
		beyon = sqlitefile.OriginBeyondEnd
		under = sqlitefile.OriginDBUnderWAL
		rolld = sqlitefile.OriginDBRolledBack
		sup   = sqlitefile.OriginWALSuperseded
		unc   = sqlitefile.OriginWALUncommitted
		stale = sqlitefile.OriginWALStale
		unver = sqlitefile.OriginWALUnverified
		jb    = sqlitefile.OriginJournalBefore
	)
	committed, uncommitted := sqlitefile.FrameCommitted, sqlitefile.FrameUncommitted
	trunc := &sqlitefile.JournalProv{Note: sqlitefile.JournalNoteBeyondInitialSize, Hot: true, Applied: true, Nonce: 0xdeadbeef}
	return []histWant{
		{origin: live, num: 1, loc: dbLoc(1)},
		{origin: live, num: 2, loc: walLoc(4)},
		{origin: live, num: 3, loc: walLoc(3)},
		{origin: live, num: 4, loc: dbLoc(4)},
		{origin: leaf, num: 6, loc: dbLoc(6)},
		{origin: leaf, num: 7, loc: jloc(2)},
		{origin: leaf, num: 8, loc: dbLoc(8)},
		{origin: leaf, num: 9, loc: jloc(1)},
		{origin: trunk, num: 5, loc: dbLoc(5)},
		{origin: orph, num: 10, loc: dbLoc(10)},
		{origin: orph, num: 11, loc: dbLoc(11)},
		{origin: beyon, num: 12, loc: dbLoc(12)},
		{origin: beyon, num: 13, loc: dbLoc(13)},
		{origin: under, num: 2, loc: dbLoc(2)},
		{origin: under, num: 3, loc: dbLoc(3)},
		{origin: rolld, num: 7, loc: dbLoc(7), journal: rb(2)},
		{origin: rolld, num: 9, loc: dbLoc(9), journal: rb(1)},
		{origin: rolld, num: 14, loc: dbLoc(14), journal: trunc},
		{origin: rolld, num: 15, loc: dbLoc(15), journal: trunc},
		{origin: sup, num: 3, loc: walLoc(1), wal: wp(1, committed, 0, "")},
		{origin: sup, num: 20, loc: walLoc(2), wal: wp(2, committed, 0, "page-beyond-commit-size")},
		{origin: unc, num: 4, loc: walLoc(5), wal: wp(5, uncommitted, 0, "")},
		{origin: stale, num: 2, loc: walLoc(9), wal: wp(9, sqlitefile.FrameStale, 2, "")},
		{origin: stale, num: 4, loc: walLoc(8), wal: wp(8, sqlitefile.FrameStale, 2, "")},
		{origin: unver, num: 2, loc: walLoc(7), wal: wp(7, sqlitefile.FrameDetached, 1, "")},
		{origin: unver, num: 3, loc: walLoc(6), wal: wp(6, sqlitefile.FrameBroken, 1, "")},
		{origin: jb, num: 6, loc: jloc(4), journal: jp(4, false)},
		{origin: jb, num: 7, loc: jloc(0), journal: jp(0, false)},
		{origin: jb, num: 7, loc: jloc(2), journal: jp(2, true)},
		{origin: jb, num: 8, loc: jloc(3), journal: jp(3, false)},
		{origin: jb, num: 9, loc: jloc(1), journal: jp(1, true)},
	}
}

func describe(p sqlitefile.PageImage) string {
	s := fmt.Sprintf("origin %d page %d loc %+v", p.Origin, p.Number, p.Loc)
	if p.WAL != nil {
		s += fmt.Sprintf(" wal %+v", *p.WAL)
	}
	if p.Journal != nil {
		s += fmt.Sprintf(" journal %+v", *p.Journal)
	}
	return s
}

func matches(p sqlitefile.PageImage, w histWant) bool {
	if p.Origin != w.origin || p.Number != w.num || p.Loc != w.loc {
		return false
	}
	if (p.WAL == nil) != (w.wal == nil) || (p.Journal == nil) != (w.journal == nil) {
		return false
	}
	return (w.wal == nil || *p.WAL == *w.wal) && (w.journal == nil || *p.Journal == *w.journal)
}

// TestHistoryPagesOrigins: the master scenario yields every origin, each page
// image exactly once, with the exact Loc and provenance, in the documented order.
func TestHistoryPagesOrigins(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	got := collectPages(t, h)
	want := m.expected(t, d)
	if len(got) != len(want) {
		for _, p := range got {
			t.Log(describe(p))
		}
		t.Fatalf("%d images, want %d", len(got), len(want))
	}
	for i := range want {
		if !matches(got[i], want[i]) {
			t.Errorf("image %d:\n got  %s\n want %+v (wal %+v journal %+v)", i, describe(got[i]), want[i], want[i].wal, want[i].journal)
		}
	}
	sum, err := h.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[sqlitefile.Origin]int64{}
	var liveSpans int64
	for _, w := range want {
		counts[w.origin]++
	}
	for _, p := range got {
		if p.Origin == sqlitefile.OriginLiveBTree {
			for _, s := range p.Spans {
				liveSpans += int64(s.Length)
			}
		}
	}
	if !reflect.DeepEqual(sum.PagesByOrigin, counts) {
		t.Errorf("PagesByOrigin %v, want %v", sum.PagesByOrigin, counts)
	}
	if sum.FreePages != 5 || sum.FreePagesZeroed != 1 || sum.LiveSpanBytes != liveSpans || liveSpans == 0 {
		t.Errorf("summary %+v, live span bytes %d", sum, liveSpans)
	}
}

// fingerprint hashes the whole image sequence.
func fingerprint(t testing.TB, h *sqlitefile.Hist) [32]byte {
	t.Helper()
	hs := sha256.New()
	for _, p := range collectPages(t, h) {
		fmt.Fprintf(hs, "%s|%v\n", describe(p), p.Spans)
	}
	var out [32]byte
	copy(out[:], hs.Sum(nil))
	return out
}

// TestHistoryOrderDeterministic: 50 repetitions, each with fresh Open calls and
// a shuffled attach order, give one hash: the order never depends on map
// iteration or on the order the companions were attached.
func TestHistoryOrderDeterministic(t *testing.T) {
	m := newHistMaster(t)
	var first [32]byte
	for i := range 50 {
		d := m.open(t, sqlitefile.Options{}, i%3 != 0)
		h := d.History()
		fp := fingerprint(t, h)
		h.Release()
		if i == 0 {
			first = fp
			if len(collectPages(t, d.History())) == 0 {
				t.Fatal("no images")
			}
		} else if fp != first {
			t.Fatalf("repetition %d: the image sequence changed", i)
		}
	}
}

// TestHistoryNeverChangesLive: History, Pages and Summary leave everything the
// live view shows as it was.
func TestHistoryNeverChangesLive(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	dump := func() (rows [][]any, info sqlitefile.Info, lay *sqlitefile.Layout, pages [][]byte) {
		v := d.Live()
		defer v.Release()
		rows = liveRows(t, v)
		info = v.Info()
		l, err := v.Layout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		lc := *l
		lc.Class, lc.Owner = slices.Clone(l.Class), slices.Clone(l.Owner)
		lay = &lc
		for pg := uint32(1); pg <= v.Addressable(); pg++ {
			p, err := v.ReadPage(pg)
			if err != nil {
				t.Fatal(err)
			}
			pages = append(pages, p.Data)
		}
		return
	}
	r1, i1, l1, p1 := dump()
	h := d.History()
	n := len(collectPages(t, h))
	if _, err := h.Summary(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Release()
	if n == 0 {
		t.Fatal("no images")
	}
	r2, i2, l2, p2 := dump()
	if !reflect.DeepEqual(r1, r2) || !reflect.DeepEqual(i1, i2) || !reflect.DeepEqual(l1, l2) || !reflect.DeepEqual(p1, p2) {
		t.Error("the live state changed after History")
	}
}

// TestHistoryLocReproducesPageBytes: Bytes() of every image equals the bytes of
// Loc.File at Loc.Offset, one page long.
func TestHistoryLocReproducesPageBytes(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	file := map[sqlitefile.FileKind][]byte{sqlitefile.FileDB: m.db, sqlitefile.FileWAL: m.wal, sqlitefile.FileJournal: m.journal}
	n := 0
	for _, p := range collectPages(t, h) {
		got, err := p.Bytes()
		if err != nil {
			t.Fatalf("%s: %v", describe(p), err)
		}
		src := file[p.Loc.File]
		want := src[p.Loc.Offset : p.Loc.Offset+hps]
		if !bytes.Equal(got, want) {
			t.Errorf("%s: Bytes() differs from the file", describe(p))
		}
		got[0] ^= 0xff // a copy: the next call is unaffected
		again, _ := p.Bytes()
		if !bytes.Equal(again, want) {
			t.Errorf("%s: Bytes() is not a copy", describe(p))
		}
		n++
	}
	if n == 0 {
		t.Fatal("no images")
	}
}

// TestDBRolledBackIsTheRawPage: the image is the database file's page while
// Live() serves the journal's page for the same number.
func TestDBRolledBackIsTheRawPage(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	h := d.History()
	t.Cleanup(h.Release)
	v := d.Live()
	defer v.Release()
	seen := 0
	for _, p := range collectPages(t, h) {
		if p.Origin != sqlitefile.OriginDBRolledBack {
			continue
		}
		seen++
		got, err := p.Bytes()
		if err != nil || !bytes.Equal(got, m.db[(p.Number-1)*hps:p.Number*hps]) {
			t.Errorf("page %d: not the raw database page (%v)", p.Number, err)
		}
		if p.Number <= 9 {
			live, err := v.ReadPage(p.Number)
			if err != nil || bytes.Equal(live.Data, got) || live.Loc.File != sqlitefile.FileJournal {
				t.Errorf("page %d: Live() serves %+v (%v), want the journal's page", p.Number, live.Loc, err)
			}
		}
	}
	if seen != 4 {
		t.Errorf("%d rolled-back images", seen)
	}
}

// TestHistoryOriginOrderAcrossGenerations: images of a stale generation that lie
// at higher slots sort by the generation's Age, not by slot.
func TestHistoryOriginOrderAcrossGenerations(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a)")
	tb.Insert(1, "x")
	db := walMode(b.Bytes())
	w := b.NewWAL(false, 90, 1, 0) // age 10, left at slots 7..9
	for i := range 9 {
		w.Frame(2, patPage(byte(i)), 0)
	}
	w.Reset(50, 2) // age 50, left at slots 4..6
	for i := range 6 {
		w.Frame(2, patPage(byte(0x10+i)), 0)
	}
	w.Reset(100, 3) // the current generation: three frames, committed
	w.Frame(2, patPage(0x21), 0)
	w.Frame(3, patPage(0x22), 0)
	w.Frame(2, b.PageBytes(2), 2)
	wal := w.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal)))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Generations) != 3 || info.Generations[1].Age != 50 || info.Generations[2].Age != 10 {
		t.Fatalf("generations %+v", info.Generations)
	}
	h := d.History()
	t.Cleanup(h.Release)
	var got []uint32
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginWALStale && p.Number == 2 {
			got = append(got, p.WAL.Frame)
		}
	}
	// age 10 (slots 7..9) before age 50 (slots 4..6); inside a generation by slot
	if want := []uint32{7, 8, 9, 4, 5, 6}; !slices.Equal(got, want) {
		t.Errorf("stale frames of page 2 in order %v, want %v", got, want)
	}
}

// TestHistoryWithoutCompanionFilesOnlyFreelistOrphanSpans: without a WAL or a
// journal the history holds only live pages (for their spans), freelist pages,
// orphans and pages beyond the end, none with WAL or journal provenance.
func TestHistoryWithoutCompanionFilesOnlyFreelistOrphanSpans(t *testing.T) {
	data, _ := orphanDB(t, 5)
	d, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	imgs := collectPages(t, h)
	if len(imgs) == 0 {
		t.Fatal("no images")
	}
	orph := 0
	for _, p := range imgs {
		switch p.Origin {
		case sqlitefile.OriginLiveBTree, sqlitefile.OriginFreelistLeaf, sqlitefile.OriginFreelistTrunk, sqlitefile.OriginOrphan, sqlitefile.OriginBeyondEnd:
		default:
			t.Errorf("origin %d without companion files: %s", p.Origin, describe(p))
		}
		if p.WAL != nil || p.Journal != nil || p.Loc.File != sqlitefile.FileDB {
			t.Errorf("provenance of a database page: %s", describe(p))
		}
		if p.Origin == sqlitefile.OriginOrphan {
			orph++
		}
	}
	if orph < 5 {
		t.Errorf("%d orphans, want at least 5", orph)
	}
}
