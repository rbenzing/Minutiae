package sqlitefile_test

// Task 9 review fixes (plan 3I, Task 10 step 0b): journal header size, gap pages,
// per-segment nonces, the super-journal name limit, the overlay size guard and
// the Live path of the journal fuzzer.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"unsafe"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestJournalUnder512BytesIsNotValid: the engine checks the first header against
// its own 512-byte sector, so a journal of fewer bytes is never played back,
// whatever its sector field says; one of 512 bytes is.
func TestJournalUnder512BytesIsNotValid(t *testing.T) {
	small := newJ(512, 32, 7, 3).Bytes()
	if len(small) != 32 {
		t.Fatalf("%d bytes", len(small))
	}
	s := scanJ(t, small, 512, sqlitefile.Options{})
	if s.Info.Applied || s.Info.HeaderValid || s.Info.NotAppliedReason != "header-invalid" || !jwarns(s, sqlitefile.WarnJournalHeaderInvalid) {
		t.Errorf("32 bytes: %+v %v", s.Info, jcodes(s))
	}
	full := append(bytes.Clone(small), make([]byte, 512-len(small))...)
	s = scanJ(t, full, 512, sqlitefile.Options{})
	if !s.Info.HeaderValid {
		t.Errorf("512 bytes: %+v", s.Info)
	}
	s = scanJ(t, full[:511], 512, sqlitefile.Options{})
	if s.Info.HeaderValid || s.Info.Applied {
		t.Errorf("511 bytes: %+v", s.Info)
	}
}

// TestJournalSegmentsHaveTheirOwnNonce: the engine writes a fresh nonce in every
// header; each segment's records are checked with their own.
func TestJournalSegmentsHaveTheirOwnNonce(t *testing.T) {
	j := newJ(512, 512, 0x11, 9)
	j.Record(2, jpg(512, 1))
	j.Record(3, jpg(512, 2))
	j.NewSegmentNonce(0x22)
	j.Record(4, jpg(512, 3))
	j.NewSegmentNonce(0x33)
	j.Record(5, jpg(512, 4))
	s := scanJ(t, j.Bytes(), 512, sqlitefile.Options{})
	if len(s.Records) != 4 || s.Info.RecordsBadChecksum != 0 || len(s.Info.Segments) != 3 || s.Info.AppliedRecords != 4 {
		t.Errorf("%+v", s.Info)
	}
}

// TestJournalLongSuperNameIsNotTheTrailerRegion: a trailer whose name is as long
// as a record must not be read as one (the records end where the trailer
// begins), and a name past the engine limit is not a trailer at all.
func TestJournalLongSuperNameIsNotTheTrailerRegion(t *testing.T) {
	build := func(n int) []byte {
		j := newJ(1024, 512, 7, 9)
		j.Record(2, jpg(1024, 1))
		j.Record(3, jpg(1024, 2))
		j.SetHeader(0xffffffff, 7, 9, 512, 1024)
		j.SuperJournal(longSuperName(n))
		return j.Bytes()
	}
	s := scanJ(t, build(superNameMax), 1024, sqlitefile.Options{SuperJournalPresent: true})
	if len(s.Records) != 2 || !s.Info.HasSuperJournal || !s.Info.Applied {
		t.Errorf("name at the limit: %d records %+v", len(s.Records), s.Info)
	}
	s = scanJ(t, build(superNameMax+1), 1024, sqlitefile.Options{})
	if s.Info.HasSuperJournal || !s.Info.Applied {
		t.Errorf("name past the limit: %+v", s.Info)
	}
}

// TestLiveWarnsOfGapPagesTheJournalClaims: when the journal initial size is above
// the file and no record supplies some of the extra pages, those pages are
// unavailable (never zero-filled) and a live-pages-unavailable warning names the
// count; with every extra page supplied, or no extra page, there is none.
func TestLiveWarnsOfGapPagesTheJournalClaims(t *testing.T) {
	gap := func(w []sqlitefile.Warning) (sqlitefile.Warning, bool) {
		for _, x := range w {
			if x.Code == sqlitefile.WarnLivePagesUnavailable && x.File == sqlitefile.FileDB {
				return x, true
			}
		}
		return sqlitefile.Warning{}, false
	}
	j := newJfix()
	db, journal, _ := gapJournal([]uint32{10})(j, "")
	_, v, _ := attachJ(t, db, journal, sqlitefile.Options{})
	w, ok := gap(v.Warnings())
	if !ok || w.Page != 8 || !bytes.Contains([]byte(w.Msg), []byte("2 pages")) {
		t.Errorf("warning %+v %v", w, ok)
	}
	if _, err := v.ReadPage(8); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("page 8: %v", err)
	}
	if _, err := v.ReadPage(10); err != nil {
		t.Errorf("page 10: %v", err)
	}
	for name, build := range map[string]buildFn{
		"all extra pages supplied": gapJournal([]uint32{8, 9, 10}),
		"no extra page":            func(j *jfix, _ string) ([]byte, []byte, []byte) { return j.dbAfter, j.journal(512, -1).Bytes(), nil },
	} {
		j := newJfix()
		db, journal, _ := build(j, "")
		_, v, _ := attachJ(t, db, journal, sqlitefile.Options{})
		if w, ok := gap(v.Warnings()); ok {
			t.Errorf("%s: unexpected warning %+v", name, w)
		}
	}
}

// TestJournalOverlayEndsAtTheInitialSize: with a WAL on top that grows the file,
// the pages above the journal initial size that no frame holds are not served
// from the database file the rollback truncated; the last page of the initial
// size is.
func TestJournalOverlayEndsAtTheInitialSize(t *testing.T) {
	j := newJfix()
	db := append(bytes.Clone(j.dbAfter), jpg(ovPS, 0x51)...)
	db = walMode(append(db, jpg(ovPS, 0x52)...)) // 9 pages, the journal says 7
	w := j.f.b.NewWAL(false, 1, 2, 0)
	w.Frame(2, db[ovPS:2*ovPS], 9) // commits 9 pages, rewriting page 2 only
	journal := j.journal(512, -1).Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	wal := w.Bytes()
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	v := d.Live()
	defer v.Release()
	if v.Addressable() != 9 {
		t.Fatalf("addressable %d", v.Addressable())
	}
	for pg, want := range map[uint32]bool{7: true, 8: false, 9: false} {
		_, err := v.ReadPage(pg)
		if (err == nil) != want || (err != nil && !errors.Is(err, sqlitefile.ErrPageUnavailable)) {
			t.Errorf("ReadPage(%d): %v, want served %v", pg, err, want)
		}
		if got := sqlitefile.ViewHas(v, pg); got != want {
			t.Errorf("has(%d) = %v, want %v", pg, got, want)
		}
	}
}

// TestJournalRecordCostCoversTheRealRecord: the budget charge per scanned record
// is at least three times the size of a record (the record, and the old and new
// backing arrays while append grows).
func TestJournalRecordCostCoversTheRealRecord(t *testing.T) {
	if got, size := int64(sqlitefile.JournalRecordCost), int64(unsafe.Sizeof(sqlitefile.JournalRecord{})); got < 3*size {
		t.Errorf("cost %d per record, a record is %d bytes", got, size)
	}
}

var fuzzLiveBase = func() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 30; i++ {
		tb.Insert(i, longText(60, byte(i)))
	}
	return b.Bytes()
}()

// liveOverJournal attaches data as the journal (and a one-frame WAL) of the base
// database and reads through both views: nothing may panic, and no read may end
// in an internal error.
func liveOverJournal(t *testing.T, data []byte) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(fuzzLiveBase), int64(len(fuzzLiveBase)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachJournal(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("AttachJournal: %v", err)
	}
	w := sqlitetest.New(sqlitetest.Options{PageSize: 512}).NewWAL(false, 1, 2, 0)
	w.Frame(2, fuzzLiveBase[512:1024], 3)
	wal := w.Bytes()
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatalf("AttachWAL: %v", err)
	}
	for _, v := range []*sqlitefile.View{d.Live(), d.AsFound()} {
		for pg := uint32(1); pg <= min(v.Addressable(), 40); pg++ {
			if _, err := v.ReadPage(pg); errors.Is(err, sqlitefile.ErrInternal) {
				t.Fatalf("ReadPage(%d): %v", pg, err)
			}
		}
		tbl, err := v.Table(context.Background(), "t")
		if errors.Is(err, sqlitefile.ErrInternal) {
			t.Fatalf("Table: %v", err)
		}
		if err == nil {
			n := 0
			if err := tbl.Rows(context.Background(), func(sqlitefile.Row) bool { n++; return n < 200 }); errors.Is(err, sqlitefile.ErrInternal) {
				t.Fatalf("Rows: %v", err)
			}
		}
		v.Release()
	}
}

// TestLiveOverFuzzSeeds runs the Live path of the fuzzer on seeds, so the path is
// exercised by every test run.
func TestLiveOverFuzzSeeds(t *testing.T) {
	good := newJ(512, 512, 0xabcdef, 3)
	good.Record(2, jpg(512, 1))
	good.Record(3, jpg(512, 2))
	liveOverJournal(t, good.Bytes())
	liveOverJournal(t, newJ(512, 32, 7, 3).Bytes())
	liveOverJournal(t, []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7})
}
