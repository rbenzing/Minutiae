package sqlitedb_test

import (
	"encoding/binary"
	"fmt"
	"slices"
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

func (s walScenario) patched(t testing.TB, mutate func(b *sqlitetest.Builder, w *sqlitetest.WAL, s walScenario)) []byte {
	t.Helper()
	b := companionBase(t)
	snap0 := b.Snapshot()
	w := b.NewWAL(false, 0x11, 0x22, 0)
	companionRewrite(b, "one")
	b.CommitTo(w, snap0)
	snap1 := b.Snapshot()
	companionRewrite(b, "two")
	b.CommitTo(w, snap1)
	mutate(b, w, s)
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
			return s.patched(t, func(_ *sqlitetest.Builder, w *sqlitetest.WAL, s walScenario) {
				w.PatchFrame(s.txn1Frames+1, 8, 0xde, 0xad, 0xbe, 0xef) // salt-1 of the first frame of txn 2
			})
		}, "wal-frames-not-applied"},
		{"mismatched-salt-2", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(_ *sqlitetest.Builder, w *sqlitetest.WAL, s walScenario) {
				w.PatchFrame(s.txn1Frames+1, 12, 0xde, 0xad, 0xbe, 0xef)
			})
		}, "wal-frames-not-applied"},
		{"frames-past-commit", func(t *testing.T, _ walScenario) []byte { return buildWAL(t, true).wal }, "wal-frames-not-applied"},
		{"torn-tail", func(_ *testing.T, s walScenario) []byte { return s.wal[:len(s.wal)-100] }, "wal-torn-tail"},
		// B90: checksum-valid frames that name a page the database cannot have.
		{"frame-page-0", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(b *sqlitetest.Builder, w *sqlitetest.WAL, _ walScenario) {
				w.Frame(0, b.PageBytes(2), b.Snapshot().Pages())
			})
		}, ""},
		{"frame-page-beyond-the-database", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(b *sqlitetest.Builder, w *sqlitetest.WAL, _ walScenario) {
				w.Frame(b.Snapshot().Pages()+50, b.PageBytes(2), b.Snapshot().Pages())
			})
		}, ""},
		// B90: a damaged b-tree page inside a checksum-valid, committed frame.
		{"damaged-page-in-a-valid-frame", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(b *sqlitetest.Builder, w *sqlitetest.WAL, _ walScenario) {
				leaf := b.Object("t").Leaves()[1]
				dmg := slices.Clone(b.PageBytes(leaf))
				dmg[3], dmg[4] = 0, 0 // cell count 0: the engine calls the page corrupt
				w.Frame(leaf, dmg, b.Snapshot().Pages())
			})
		}, "btree-shape"},
		{"flipped-checksum-in-txn2", func(t *testing.T, s walScenario) []byte {
			return s.patched(t, func(_ *sqlitetest.Builder, w *sqlitetest.WAL, s walScenario) {
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
	journal := companionJournal
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

// companionJournal is a rollback journal holding the pre-images (snap) of the
// pages b changed since; badAt > 0 gives that record a wrong checksum.
func companionJournal(b *sqlitetest.Builder, snap *sqlitetest.Image, nonce uint32, badAt int) *sqlitetest.Journal {
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

// companionCheck opens the files and requires, in every case, a result that
// does not hide the damage:
//   - Open fails: the error is one of the layer's typed errors (exercise checks
//     every error on the read surface) and its text is not empty.
//   - Open succeeds: the named warning (code, when given) is among the warnings;
//     and where the engine accepts the files the layer's rows equal the engine's;
//     where the engine refuses them the layer must have warned (the files are
//     never presented as clean).
func companionCheck(t *testing.T, db, wal, jrn []byte, code string) {
	t.Helper()
	o := exercise(t, filesOf(db, wal, jrn), 128<<20)
	t.Logf("open=%v rows=%v warnings=%d all=%d codes=%v", o.openErr, o.rows, o.warnings, o.allWarn, o.codes)
	if o.openErr != nil {
		if o.openErr.Error() == "" {
			t.Error("Open failed with an error that says nothing")
		}
		return
	}
	if code != "" && !o.codes[code] {
		t.Errorf("no %q warning among %v", code, o.codes)
	}
	d := openBytes(t, db, wal, jrn, bigBudget())
	eng, _ := engineOnCopy(t, db, wal, jrn)
	if _, refused := compareWithEngine(t, eng, d); refused != nil {
		t.Logf("the engine refuses these files (%v)", refused)
		if o.allWarn == 0 {
			t.Errorf("the engine refuses the files (%v) but the layer opened them with no warning", refused)
		}
	}
}

// B90: a database header whose page count lies (more pages than the file
// holds, fewer, zero) is reported or equals the engine's reading; rows are never
// lost silently.
func TestSQLiteDBHostileHeaderPageCount(t *testing.T) {
	cases := []struct {
		name  string
		count func(pages uint32) uint32
	}{
		{"larger-than-the-file", func(pages uint32) uint32 { return pages + 40 }},
		{"smaller-than-the-file", func(pages uint32) uint32 { return pages / 2 }},
		{"zero", func(uint32) uint32 { return 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := companionBase(t).Bytes()
			pages := binary.BigEndian.Uint32(db[28:])
			binary.BigEndian.PutUint32(db[28:], c.count(pages))
			companionCheck(t, db, nil, nil, "")
		})
	}
}

// B85: a WAL database that also has a hot rollback journal (a real device state).
// The files are compared with the engine where it accepts them; the journal and
// WAL together are named by a warning and Info reports both companions.
func TestOpenWALAndHotJournalTogether(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("damaged-journal-%v", damaged), func(t *testing.T) {
			b := companionBase(t)
			db := b.Bytes()
			snap0 := b.Snapshot()
			w := b.NewWAL(false, 0x11, 0x22, 0)
			companionRewrite(b, "one")
			b.CommitTo(w, snap0)
			if changedPages(b, snap0) < 4 {
				t.Fatal("fewer than four changed pages")
			}
			badAt := 0
			if damaged {
				badAt = 3
			}
			jrn := companionJournal(b, snap0, 0x1234, badAt).Bytes()
			code := "journal-and-wal"
			companionCheck(t, db, w.Bytes(), jrn, code)
			d := openBytes(t, db, w.Bytes(), jrn, bigBudget())
			in := d.Info()
			if in.WAL == nil || in.Journal == nil {
				t.Errorf("Info reports WAL %v and journal %v, want both", in.WAL, in.Journal)
			}
		})
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
