package sqlitefile_test

// Engine oracle, rollback journal (plan 3I, Task 13): the engine writes a hot
// journal (a small page cache and an oversized transaction), the files are
// copied while the transaction is open, and the engine's view of the copy (it
// rolls the journal back) is compared with Live(); AsFound() shows the file as
// the interrupted transaction left it and must differ.

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

type hotFixture struct {
	db, journal []byte
	ps          int
	baseline    map[string][]engineRow // what the engine reads once the journal is rolled back
	sector      int
}

// newHotFixture runs an oversized transaction in rollback mode and copies the
// database and its journal in the middle of it.
func newHotFixture(t *testing.T, ps int) *hotFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.db")
	e := openEngine(t, path)
	mustExec(t, e, "pragma page_size="+fmt.Sprint(ps))
	mustExec(t, e, "pragma journal_mode=delete")
	mustExec(t, e, "create table a(id integer primary key, v)")
	mustExec(t, e, "create table b(id integer primary key, v)")
	mustExec(t, e, rowsInsert("a", 1, 1500, 150))
	mustExec(t, e, rowsInsert("b", 1, 100, 400))
	baseline := engineDumpRows(t, e)
	mustExec(t, e, "pragma cache_size=10")
	mustExec(t, e, "pragma cache_spill=10")
	tx, err := e.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		"update a set v = v || 'changed-by-the-open-transaction'",
		"delete from b where id % 2 = 0",
		rowsInsert("b", 1000, 1300, 500),
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	mode := copyFiles(t, dir, path, path+"-journal")
	if mode != copyDirect {
		t.Logf("copy-while-open fallback used: %s", mode)
	}
	h := &hotFixture{ps: ps, baseline: baseline}
	if h.db, err = os.ReadFile(filepath.Join(dir, "h.db")); err != nil {
		t.Fatal(err)
	}
	if h.journal, err = os.ReadFile(filepath.Join(dir, "h.db-journal")); err != nil {
		t.Fatal(err)
	}
	h.sector = int(binary.BigEndian.Uint32(h.journal[20:]))
	return h
}

// jrec is one record of the journal as the engine laid it out.
type jrec struct {
	off   int // of the page number
	nonce uint32
	seg   int
}

// records walks the journal by its own headers, independently of the library: a
// segment is a sector-sized header (nRec at 8, nonce at 12) followed by nRec
// records (page number, page data, checksum); the next segment starts at the
// next sector boundary.
func (h *hotFixture) records() []jrec {
	var out []jrec
	step := 4 + h.ps + 4
	off, seg := 0, 0
	for off+h.sector <= len(h.journal) {
		if !bytes.Equal(h.journal[off:off+8], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
			break // a segment whose header was never synced: the engine does not read past it
		}
		n := binary.BigEndian.Uint32(h.journal[off+8:])
		nonce := binary.BigEndian.Uint32(h.journal[off+12:])
		p := off + h.sector
		cnt := 0
		for (n == 0xffffffff || uint32(cnt) < n) && p+step <= len(h.journal) {
			out = append(out, jrec{off: p, nonce: nonce, seg: seg})
			p += step
			cnt++
		}
		off = (p + h.sector - 1) / h.sector * h.sector
		seg++
	}
	return out
}

func libraryLive(t *testing.T, db, wal, journal []byte) (map[string][]engineRow, *sqlitefile.DB, error) {
	t.Helper()
	d := openLibrary(t, db, wal, journal)
	v := d.Live()
	defer v.Release()
	out, err := liveDumpRowsErr(t, v)
	return out, d, err
}

// TestLiveMatchesEngineHotJournal (inverted by ruling D1): Live() equals the
// engine's view of the copy, which rolled the journal back; AsFound() differs
// from it.
func TestLiveMatchesEngineHotJournal(t *testing.T) {
	for _, ps := range []int{1024, 4096} {
		t.Run(fmt.Sprint("ps", ps), func(t *testing.T) {
			h := newHotFixture(t, ps)
			offs := h.records()
			if len(offs) < 8 {
				t.Fatalf("the journal holds only %d records", len(offs))
			}
			want, err := engineCopyDump(t, "h.db", h.db, nil, h.journal)
			if err != nil {
				t.Fatalf("engine: %v", err)
			}
			if diff, _ := diffDumps(want, h.baseline); diff != "" {
				t.Fatalf("the engine did not roll back to the baseline: %s", diff)
			}
			got, d, lerr := libraryLive(t, h.db, nil, h.journal)
			if lerr != nil {
				t.Fatalf("Live(): %v", lerr)
			}
			if diff, _ := diffDumps(got, want); diff != "" {
				t.Fatalf("Live() differs from the engine: %s", diff)
			}
			j := d.Status().Journal
			if j == nil || !j.Hot || !j.Applied {
				t.Fatalf("journal status %+v", j)
			}
			// the discriminating half: the file as found is not the engine's view
			af := d.AsFound()
			asFound, aerr := liveDumpRowsErr(t, af)
			af.Release()
			if aerr == nil {
				if diff, _ := diffDumps(asFound, want); diff == "" {
					t.Error("AsFound() equals the rolled-back state: the discriminating half is vacuous")
				}
			}
			t.Logf("journal: %d records, %d segment(s), nRec field %#x", len(offs), len(j.Segments), binary.BigEndian.Uint32(h.journal[8:]))
			// the checksum of record k is wrong: playback stops before it. The
			// first record of every segment and the last record are tried; a
			// copy the engine cannot read after the partial rollback is not compared.
			compared, refused := 0, 0
			var ks []int
			for i, r := range offs {
				if i == 0 || r.seg != offs[i-1].seg || i == len(offs)-1 {
					ks = append(ks, i)
				}
			}
			for _, k := range ks {
				jm := bytes.Clone(h.journal)
				jm[offs[k].off+4+h.ps+3] ^= 0x01
				switch compareJournalCopy(t, fmt.Sprintf("checksum of record %d flipped", k), h.db, nil, jm) {
				case copyCompared:
					compared++
				case copyEngineRefused:
					refused++
				}
			}
			t.Logf("checksum flips: %d of %d compared equal, %d unreadable by the engine", compared, len(ks), refused)
			if compared == 0 {
				t.Error("no checksum flip left a copy the engine can read: the half is vacuous")
			}
			// nRec patched to 0xffffffff: the records run to the end of the file
			jm := bytes.Clone(h.journal)
			binary.BigEndian.PutUint32(jm[8:], 0xffffffff)
			compareJournalCopy(t, "nRec 0xffffffff", h.db, nil, jm)
			// a journal cut inside its last record
			compareJournalCopy(t, "a journal cut inside its last record", h.db, nil, bytes.Clone(h.journal)[:len(h.journal)-h.ps/2])
			if len(j.Segments) > 1 {
				t.Logf("multi-segment journal produced by the driver (%d segments) and compared above", len(j.Segments))
			} else {
				t.Logf("[M] the driver wrote one segment: multi-segment journals are covered by TestEngineJournalRules")
			}
		})
	}
}

// TestJournalChecksumMatchesEngine: every record of an engine-written journal
// verifies with JournalChecksum (the nonce is at byte 12 of the header).
func TestJournalChecksumMatchesEngine(t *testing.T) {
	for _, ps := range []int{512, 1024, 4096} {
		h := newHotFixture(t, ps)

		offs := h.records()
		if len(offs) == 0 {
			t.Fatalf("ps %d: no records", ps)
		}
		for i, rc := range offs {
			off, nonce := rc.off, rc.nonce
			page := h.journal[off+4 : off+4+ps]
			stored := binary.BigEndian.Uint32(h.journal[off+4+ps:])
			if got := sqlitefile.JournalChecksum(nonce, page); got != stored {
				t.Fatalf("ps %d record %d (segment %d, offset %d of %d, nonce %#x): JournalChecksum %#x, the engine stored %#x", ps, i, rc.seg, off, len(h.journal), nonce, got, stored)
			}
		}
	}
}

// TestLiveMatchesEngineJournalAndWAL: a database in WAL mode with a WAL and a
// hot journal, fed to the engine, validates the order the library follows: the
// rollback first, the WAL's committed frames over it.
func TestLiveMatchesEngineJournalAndWAL(t *testing.T) {
	for _, withPage3Record := range []bool{false, true} {
		t.Run(fmt.Sprint("journal record for the WAL page: ", withPage3Record), func(t *testing.T) {
			r := newRollbackScen(t)
			r.j.Record(2, r.before2)
			if withPage3Record {
				r.j.Record(3, r.before3)
			}
			img := r.s.setU(1, 7)
			w := r.s.b.NewWAL(false, 0x1000, 0x1001, 0)
			w.Frame(3, img.Page(3), 3)
			db := walMode(r.db)
			want, err := engineCopyDump(t, "jw.db", db, w.Bytes(), r.j.Bytes())
			if err != nil {
				t.Fatalf("engine: %v", err)
			}
			got, d, lerr := libraryLive(t, db, w.Bytes(), r.j.Bytes())
			if lerr != nil {
				t.Fatalf("Live(): %v", lerr)
			}
			if diff, _ := diffDumps(got, want); diff != "" {
				t.Fatalf("Live() differs from the engine: %s", diff)
			}
			s := d.Status()
			if s.Journal == nil || !s.Journal.Applied || s.WAL == nil || !s.WAL.UsedByLive {
				t.Errorf("status: journal %+v wal %+v", s.Journal, s.WAL)
			}
			// the same pair without the WAL: the engine's rollback alone
			want2, err := engineCopyDump(t, "j.db", db, nil, r.j.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if diff, _ := diffDumps(want, want2); diff == "" {
				t.Error("the WAL changes nothing: the combination is vacuous")
			}
		})
	}
	_ = sql.ErrNoRows
}

const (
	copyCompared = iota
	copyEngineRefused
)

// compareJournalCopy compares Live() of (db, wal, journal) with the engine's view
// of a copy. A copy the engine cannot read (the partial rollback left it
// inconsistent) is reported as such and not compared.
func compareJournalCopy(t *testing.T, label string, db, wal, journal []byte) int {
	t.Helper()
	want, err := engineCopyDump(t, "h.db", db, wal, journal)
	if err != nil {
		return copyEngineRefused
	}
	got, _, lerr := libraryLive(t, db, wal, journal)
	if lerr != nil {
		t.Errorf("%s: the engine reads the copy, Live() fails: %v", label, lerr)
		return copyCompared
	}
	if diff, _ := diffDumps(got, want); diff != "" {
		t.Errorf("%s: %s", label, diff)
	}
	return copyCompared
}
