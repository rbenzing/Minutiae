package sqlitedb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// companionBase is a database of 60 rows over several 512-byte pages.
func companionBase(t testing.TB) *sqlitetest.Builder {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tt := b.CreateTable("t", "create table t(id integer primary key, v text)")
	for i := int64(1); i <= 60; i++ {
		tt.Insert(i, nil, fmt.Sprintf("zero-%02d-%s", i, strings.Repeat("p", 20)))
	}
	return b
}

func companionRewrite(b *sqlitetest.Builder, word string) {
	tt := b.Object("t")
	for i := int64(1); i <= 60; i++ {
		tt.Update(i, nil, fmt.Sprintf("%s-%02d-%s", word, i, strings.Repeat("p", 20)))
	}
}

// walScenario builds the committed WAL transactions and returns the database
// file (always the checkpointed base), the WAL bytes and the frame counts.
type walScenario struct {
	db, wal                []byte
	txn1Frames, txn2Frames int
}

func buildWAL(t testing.TB, uncommittedTail bool) walScenario {
	t.Helper()
	b := companionBase(t)
	db := b.Bytes()
	snap0 := b.Snapshot()
	w := b.NewWAL(false, 0x11, 0x22, 0)
	companionRewrite(b, "one")
	b.CommitTo(w, snap0)
	n1 := framesOf(w.Bytes(), 512)
	snap1 := b.Snapshot()
	companionRewrite(b, "two")
	b.CommitTo(w, snap1)
	n2 := framesOf(w.Bytes(), 512) - n1
	if uncommittedTail {
		snap2 := b.Snapshot()
		companionRewrite(b, "three")
		for i := uint32(1); i <= snap2.Pages(); i++ {
			if string(b.PageBytes(i)) != string(snap2.Page(i)) {
				w.Frame(i, b.PageBytes(i), 0)
			}
		}
	}
	return walScenario{db: db, wal: w.Bytes(), txn1Frames: n1, txn2Frames: n2}
}

func framesOf(wal []byte, pageSize int) int { return (len(wal) - 32) / (24 + pageSize) }

func (s walScenario) patched(t testing.TB, mutate func(w *sqlitetest.WAL, s walScenario)) []byte {
	t.Helper()
	b := companionBase(t)
	snap0 := b.Snapshot()
	w := b.NewWAL(false, 0x11, 0x22, 0)
	companionRewrite(b, "one")
	b.CommitTo(w, snap0)
	snap1 := b.Snapshot()
	companionRewrite(b, "two")
	b.CommitTo(w, snap1)
	mutate(w, s)
	return w.Bytes()
}

// TestSQLiteDBHostileCompanions: damaged WAL and journal files. Where the
// engine accepts the files the layer's rows equal the engine's; where it does
// not, the layer refuses with a typed error or warns. In every case a
// warning names the cause.
func TestSQLiteDBHostileCompanions(t *testing.T) {
	type wcase struct {
		name string
		wal  func(t *testing.T, s walScenario) []byte
		code string // a warning code that names the cause
	}
	wcases := []wcase{
		{"clean", func(_ *testing.T, s walScenario) []byte { return s.wal }, ""},
		{"mismatched-salts", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(w *sqlitetest.WAL, s walScenario) {
				w.PatchFrame(s.txn1Frames+1, 8, 0xde, 0xad, 0xbe, 0xef) // salt-1 of the first frame of txn 2
			})
		}, "wal-frames-not-applied"},
		{"mismatched-salt-2", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(w *sqlitetest.WAL, s walScenario) {
				w.PatchFrame(s.txn1Frames+1, 12, 0xde, 0xad, 0xbe, 0xef)
			})
		}, "wal-frames-not-applied"},
		{"frames-past-commit", func(t *testing.T, _ walScenario) []byte { return buildWAL(t, true).wal }, "wal-frames-not-applied"},
		{"torn-tail", func(_ *testing.T, s walScenario) []byte { return s.wal[:len(s.wal)-100] }, "wal-torn-tail"},
		{"flipped-checksum-in-txn2", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(w *sqlitetest.WAL, s walScenario) {
				w.PatchFrame(s.txn1Frames+2, 16, 0xff) // the frame checksum
			})
		}, "wal-frames-not-applied"},
	}
	for _, c := range wcases {
		t.Run("wal-"+c.name, func(t *testing.T) {
			s := buildWAL(t, false)
			wal := c.wal(t, s)
			companionCheck(t, s.db, wal, nil, c.code)
		})
	}

	type jcase struct {
		name string
		jrn  func(t *testing.T, b *sqlitetest.Builder, snap *sqlitetest.Image) []byte
		code string
	}
	journal := func(b *sqlitetest.Builder, snap *sqlitetest.Image, nonce uint32, badAt int) *sqlitetest.Journal {
		j := b.NewJournal(512, nonce, snap.Pages())
		n := 0
		for p := uint32(1); p <= snap.Pages(); p++ {
			if string(b.PageBytes(p)) == string(snap.Page(p)) {
				continue
			}
			n++
			if n == badAt {
				j.RawRecord(p, snap.Page(p), 0xdeadbeef)
				continue
			}
			j.Record(p, snap.Page(p))
		}
		return j
	}
	jcases := []jcase{
		{"clean", func(_ *testing.T, b *sqlitetest.Builder, s *sqlitetest.Image) []byte {
			return journal(b, s, 0x1234, 0).Bytes()
		}, "journal-hot"},
		{"bad-record-3-checksum", func(_ *testing.T, b *sqlitetest.Builder, s *sqlitetest.Image) []byte {
			return journal(b, s, 0x1234, 3).Bytes()
		}, "journal-page-invalid"},
		{"nonce-0", func(_ *testing.T, b *sqlitetest.Builder, s *sqlitetest.Image) []byte {
			return journal(b, s, 0, 0).Bytes()
		}, "journal-hot"},
		{"truncated-mid-record", func(_ *testing.T, b *sqlitetest.Builder, s *sqlitetest.Image) []byte {
			jb := journal(b, s, 0x1234, 0).Bytes()
			return jb[:len(jb)-100]
		}, "journal-hot"},
	}
	for _, c := range jcases {
		t.Run("journal-"+c.name, func(t *testing.T) {
			b := companionBase(t)
			snap := b.Snapshot()
			companionRewrite(b, "new")
			if changedPages(b, snap) < 4 {
				t.Fatal("fewer than four changed pages: record 3 would not exist")
			}
			companionCheck(t, b.Bytes(), nil, c.jrn(t, b, snap), c.code)
		})
	}
}

// companionCheck opens the files, compares them with the engine where it
// accepts them and requires the named warning.
func companionCheck(t *testing.T, db, wal, jrn []byte, code string) {
	t.Helper()
	o := exercise(t, filesOf(db, wal, jrn), 128<<20)
	t.Logf("open=%v rows=%v warnings=%d all=%d codes=%v", o.openErr, o.rows, o.warnings, o.allWarn, o.codes)
	if code != "" && !o.codes[code] && o.openErr == nil {
		t.Errorf("no %q warning among %v", code, o.codes)
	}
	if o.openErr != nil {
		return
	}
	d := openBytes(t, db, wal, jrn, bigBudget())
	eng, _ := engineOnCopy(t, db, wal, jrn)
	if _, refused := compareWithEngine(t, eng, d); refused != nil {
		t.Logf("the engine refuses these files (%v): the layer's typed result stands", refused)
	}
}

func changedPages(b *sqlitetest.Builder, snap *sqlitetest.Image) int {
	n := 0
	for p := uint32(1); p <= snap.Pages(); p++ {
		if string(b.PageBytes(p)) != string(snap.Page(p)) {
			n++
		}
	}
	return n
}
