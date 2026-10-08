package sqlitefile_test

// TestEngineJournalRules is the engine probe for the rollback journal rules of
// the Format reference ("When Live() applies a journal", "Playback rules").
// Every case builds a database in its AFTER state plus a hot journal whose
// records are the BEFORE images, hands COPIES to the real engine (it rolls the
// journal back and deletes it) and records what it did: which version of each
// row it shows (B before, A after, - missing, ? other), the size of the
// database file afterwards and whether the journal is still there. The first
// run decided every expected value below; Live() is made to agree in
// journalrules_test.go (which reuses jfix and these cases).

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const jrows = 40

// jfix is a database whose every row was rewritten by a transaction: the
// database file as found is the AFTER state, the journal holds the BEFORE
// images of the changed pages.
type jfix struct {
	f       *wfix
	s0      *sqlitetest.Image
	dbAfter []byte
	before  builderRows
	after   builderRows
	third   builderRows // rows 1..5 rewritten again, as a WAL commit does
	changed []uint32
}

func newJfix() *jfix {
	f := newWfix(jrows)
	s0 := f.b.Snapshot()
	before := f.want()
	for id := int64(1); id <= jrows; id++ {
		f.set(id, byte(id)+100, false)
	}
	j := &jfix{f: f, s0: s0, before: before, after: f.want(), changed: changedPages(s0, f.b), dbAfter: f.b.Bytes()}
	j.third = append(builderRows(nil), j.after...)
	for id := int64(1); id <= 5; id++ {
		j.third[id-1] = []any{id, id * 3, longText(100, byte(id)+50)}
	}
	if f.b.Snapshot().Pages() != s0.Pages() {
		panic("the transaction must not change the page count")
	}
	return j
}

// journal returns a journal with a record per changed page (before images),
// optionally cut to the first n records (n < 0: all).
func (j *jfix) journal(sector int, n int) *sqlitetest.Journal {
	jr := j.f.b.NewJournal(sector, 0xdeadbeef, j.s0.Pages())
	for i, pg := range j.changed {
		if n >= 0 && i >= n {
			break
		}
		jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
	}
	return jr
}

// labels classifies each of the rows by which version the engine shows.
func (j *jfix) labels(rows [][]any) string {
	byID := map[int64][]any{}
	for _, r := range rows {
		if id, ok := r[0].(int64); ok {
			byID[id] = r
		}
	}
	var b strings.Builder
	for id := int64(1); id <= jrows; id++ {
		r, ok := byID[id]
		switch {
		case !ok:
			b.WriteByte('-')
		case rowEq(r, j.before[id-1]):
			b.WriteByte('B')
		case rowEq(r, j.after[id-1]):
			b.WriteByte('A')
		case rowEq(r, j.third[id-1]):
			b.WriteByte('W')
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func rowEq(a, b []any) bool { return reflect.DeepEqual(a, b) }

// engineOutcome is what the engine did with a database and its companions.
type engineOutcome struct {
	labels      string
	pages       int64 // database file size in pages (page size ovPS) after the query
	err         string
	journalLeft bool
}

func (o engineOutcome) String() string {
	return fmt.Sprintf("%s|%d|%s|%v", o.labels, o.pages, o.err, o.journalLeft)
}

// runEngine writes db, journal and wal (nil: absent) as copies into a fresh
// directory and lets the engine open and query them.
func (j *jfix) runEngine(t testing.TB, db, journal, wal []byte) engineOutcome {
	t.Helper()
	p := filepath.Join(t.TempDir(), "j.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if journal != nil {
		if err := os.WriteFile(p+"-journal", journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if wal != nil {
		if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := openEngine(t, p)
	var out engineOutcome
	rows, err := e.Query("select rowid, a, b from t order by rowid")
	if err != nil {
		out.err = shortErr(err)
	} else {
		var got [][]any
		for rows.Next() {
			var id, a int64
			var b string
			if err := rows.Scan(&id, &a, &b); err != nil {
				out.err = shortErr(err)
				break
			}
			got = append(got, []any{id, a, b})
		}
		if err := rows.Err(); err != nil && out.err == "" {
			out.err = shortErr(err)
		}
		_ = rows.Close()
		out.labels = j.labels(got)
	}
	if st, err := os.Stat(p); err == nil {
		out.pages = st.Size() / ovPS
	}
	_, jerr := os.Stat(p + "-journal")
	out.journalLeft = jerr == nil
	return out
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return strings.ReplaceAll(s, "|", "/")
}

// journalCase is one row of the probe. build returns the files to give the
// engine (dir is a directory a named super-journal may live in).
type journalCase struct {
	name  string
	build func(j *jfix, dir string) (db, journal, wal []byte)
	want  string // labels|pages|error|journalLeft, as the engine did on the first run
}

func (j *jfix) badSum(n, at int) *sqlitetest.Journal {
	jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
	for i, pg := range j.changed {
		if i >= n {
			break
		}
		if i == at {
			jr.RawRecord(pg, bytes.Clone(j.s0.Page(pg)), 0x12345)
		} else {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
	}
	return jr
}

func junkPage() []byte { return bytes.Repeat([]byte{0xAB}, ovPS) }

type buildFn = func(j *jfix, dir string) (db, journal, wal []byte)

func journalCases() []journalCase {
	var cs []journalCase
	add := func(name string, build buildFn) {
		w, ok := journalWants[name]
		if !ok {
			panic("no recorded engine outcome for " + name)
		}
		cs = append(cs, journalCase{name, build, w})
	}
	plain := func(sector, n int) buildFn {
		return func(j *jfix, _ string) ([]byte, []byte, []byte) {
			return j.dbAfter, j.journal(sector, n).Bytes(), nil
		}
	}
	add("baseline hot journal", plain(512, -1))
	add("sector 4096", plain(4096, -1))
	add("sector 32", plain(32, -1))
	add("journal smaller than 4096 bytes: header and one record", plain(512, 1))
	add("journal smaller than 4096 bytes: header and one record, sector 32", plain(32, 1))
	add("journal smaller than 4096 bytes: header only, db longer", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		db := append(bytes.Clone(j.dbAfter), make([]byte, 3*ovPS)...)
		return db, j.journal(512, 0).Bytes(), nil
	})
	for _, k := range []int{0, 1, 3} {
		add(fmt.Sprintf("bad checksum at record %d", k), func(j *jfix, _ string) ([]byte, []byte, []byte) {
			return j.dbAfter, j.badSum(len(j.changed), k).Bytes(), nil
		})
	}
	add("record for page 0 in the middle", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for i, pg := range j.changed {
			if i == 2 {
				jr.Record(0, junkPage())
			}
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	add("record for the lock-byte page in the middle", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		const lock = (1<<30)/ovPS + 1
		jr := j.f.b.NewJournal(512, 0xdeadbeef, lock+1)
		for i, pg := range j.changed {
			if i == 2 {
				jr.Record(lock, junkPage())
			}
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	add("record above the initial size in the middle (db longer)", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		db := append(bytes.Clone(j.dbAfter), make([]byte, 3*ovPS)...)
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for i, pg := range j.changed {
			if i == 2 {
				jr.Record(j.s0.Pages()+2, junkPage())
			}
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return db, jr.Bytes(), nil
	})
	add("duplicate page: junk then real", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		jr.Record(j.changed[0], junkPage())
		for _, pg := range j.changed {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	add("duplicate page: real then junk", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.Record(j.changed[0], junkPage())
		return j.dbAfter, jr.Bytes(), nil
	})
	add("first header nRec 0, database longer than initial", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		db := append(bytes.Clone(j.dbAfter), make([]byte, 3*ovPS)...)
		jr := j.journal(512, -1)
		jr.SetHeader(0, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
		return db, jr.Bytes(), nil
	})
	add("nRec 0xffffffff", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.SetHeader(0xffffffff, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("nRec 2 with more records present", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.SetHeader(2, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("nRec larger than the records present", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.SetHeader(1000, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("journal page size 2048 against a 1024 database", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := sqlitetest.New(sqlitetest.Options{PageSize: 2048}).NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for _, pg := range j.changed {
			p := make([]byte, 2048)
			copy(p, j.s0.Page(pg))
			jr.Record(pg, p)
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	for _, sec := range []uint32{0, 3, 31, 70000, 0xffffffff} {
		add(fmt.Sprintf("header sector size %d", sec), func(j *jfix, _ string) ([]byte, []byte, []byte) {
			jr := j.journal(512, -1)
			jr.SetHeader(uint32(len(j.changed)), 0xdeadbeef, j.s0.Pages(), sec, ovPS)
			return j.dbAfter, jr.Bytes(), nil
		})
	}
	for _, ps := range []uint32{0, 100, 70000} {
		add(fmt.Sprintf("header page size %d", ps), func(j *jfix, _ string) ([]byte, []byte, []byte) {
			jr := j.journal(512, -1)
			jr.SetHeader(uint32(len(j.changed)), 0xdeadbeef, j.s0.Pages(), 512, ps)
			return j.dbAfter, jr.Bytes(), nil
		})
	}
	add("magic mismatch, first byte non-zero", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		b := j.journal(512, -1).Bytes()
		b[3] ^= 0xff
		return j.dbAfter, b, nil
	})
	add("first byte zero", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		b := j.journal(512, -1).Bytes()
		b[0] = 0
		return j.dbAfter, b, nil
	})
	add("zeroed header (persist)", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.Zero()
		return j.dbAfter, jr.Bytes(), nil
	})
	add("empty journal", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		return j.dbAfter, []byte{}, nil
	})
	add("initial pages 0", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.SetHeader(uint32(len(j.changed)), 0xdeadbeef, 0, 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("journal supplies pages beyond the database end", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		// the transaction truncated the file by two pages: their before images
		// are in the journal, and so are the changed leaves
		n := int(j.s0.Pages())
		db := bytes.Clone(j.dbAfter[:(n-2)*ovPS])
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for pg := j.s0.Pages() - 1; pg <= j.s0.Pages(); pg++ {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		for _, pg := range j.changed {
			if pg < j.s0.Pages()-1 {
				jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
			}
		}
		return db, jr.Bytes(), nil
	})
	add("later segment with its own initial size", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, 2)
		jr.NewSegment()
		for _, pg := range j.changed[2:] {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		jr.SetHeader(uint32(len(j.changed)-2), 0xdeadbeef, 2, 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("three segments", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for i, pg := range j.changed {
			if i > 0 && i%2 == 0 {
				jr.NewSegment()
			}
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	add("later segment with nRec 0", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.journal(512, 2)
		jr.NewSegment() // nRec 0: the records after it belong to no segment
		for _, pg := range j.changed[2:] {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		jr.SetHeader(0, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("super-journal named, file absent", func(j *jfix, dir string) ([]byte, []byte, []byte) {
		jr := j.journal(512, -1)
		jr.SuperJournal(filepath.ToSlash(filepath.Join(dir, "absent-super")))
		return j.dbAfter, jr.Bytes(), nil
	})
	add("super-journal named, file present", func(j *jfix, dir string) ([]byte, []byte, []byte) {
		name := filepath.ToSlash(filepath.Join(dir, "present-super"))
		if err := os.WriteFile(name, []byte("child-journal"), 0o600); err != nil { // an empty file counts as absent
			panic(err)
		}
		jr := j.journal(512, -1)
		jr.SuperJournal(name)
		return j.dbAfter, jr.Bytes(), nil
	})
	add("hot journal and a WAL", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		// the WAL commits a third version of rows 1..5 on top of the AFTER file
		f := newWfix(jrows)
		for id := int64(1); id <= jrows; id++ {
			f.set(id, byte(id)+100, false)
		}
		s1 := f.b.Snapshot()
		w := f.b.NewWAL(false, 1, 2, 0)
		for id := int64(1); id <= 5; id++ {
			f.set(id, byte(id)+50, false)
		}
		f.b.CommitTo(w, s1)
		return walMode(j.dbAfter), j.journal(512, -1).Bytes(), w.Bytes()
	})
	short := func(sector uint32, size int) buildFn {
		return func(j *jfix, _ string) ([]byte, []byte, []byte) {
			jr := j.f.b.NewJournal(512, 0xdeadbeef, 3)
			jr.SetHeader(0, 0xdeadbeef, 3, sector, ovPS)
			return j.dbAfter, jr.Bytes()[:size], nil
		}
	}
	add("journal of 32 bytes, sector 32, initial pages 3", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		return j.dbAfter, j.f.b.NewJournal(32, 0xdeadbeef, 3).Bytes(), nil
	})
	add("journal of 511 bytes, sector 32, initial pages 3", short(32, 511))
	add("journal of 512 bytes, sector 32, initial pages 3", short(32, 512))
	add("initial size above the file, record for the last extra page", gapJournal([]uint32{10}))
	add("initial size above the file, records for all extra pages", gapJournal([]uint32{8, 9, 10}))
	add("later segment with its own nonce", func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.f.b.NewJournal(512, 0xdeadbeef, j.s0.Pages())
		for i, pg := range j.changed {
			if i == 2 {
				jr.NewSegmentNonce(0x0badcafe)
			}
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	})
	for _, n := range []int{superNameMax, superNameMax + 1} {
		add(fmt.Sprintf("super-journal name of %d bytes, file absent, nRec 0xffffffff", n), func(j *jfix, _ string) ([]byte, []byte, []byte) {
			jr := j.journal(512, -1)
			jr.SetHeader(0xffffffff, 0xdeadbeef, j.s0.Pages(), 512, ovPS)
			jr.SuperJournal(longSuperName(n))
			return j.dbAfter, jr.Bytes(), nil
		})
	}
	return cs
}

func TestEngineJournalRules(t *testing.T) {
	for _, c := range journalCases() {
		t.Run(c.name, func(t *testing.T) {
			j := newJfix()
			db, journal, wal := c.build(j, t.TempDir())
			got := j.runEngine(t, db, journal, wal).String()
			if got != c.want {
				t.Errorf("engine outcome (labels|pages|error|journal left):\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// lab builds a label string: lab("B", 9, "A", 31) is nine B and 31 A.
func lab(parts ...any) string {
	var b strings.Builder
	for i := 0; i+1 < len(parts); i += 2 {
		b.WriteString(strings.Repeat(parts[i].(string), parts[i+1].(int)))
	}
	return b.String()
}

// out formats an engine outcome.
func out(labels string, pages int, err string, journalLeft bool) string {
	return engineOutcome{labels, int64(pages), err, journalLeft}.String()
}

const malformed = "database disk image is malformed (11)"

// journalWants is what the engine did on the first run of every case (each
// case is a row of the journal rules in the Format reference). Rows: seven
// leaf pages hold the 40 rows nine per leaf; the AFTER file has 7 pages.
var journalWants = map[string]string{
	"baseline hot journal": out(lab("B", 40), 7, "", false),
	"sector 4096":          out(lab("B", 40), 7, "", false),
	"sector 32":            out(lab("B", 40), 7, "", false),
	"journal smaller than 4096 bytes: header and one record":            out(lab("B", 9, "A", 31), 7, "", false),
	"journal smaller than 4096 bytes: header and one record, sector 32": out(lab("B", 9, "A", 31), 7, "", false),
	"journal smaller than 4096 bytes: header only, db longer":           out(lab("A", 40), 7, "", false),
	// playback ends at the first bad checksum: earlier records stay applied
	"bad checksum at record 0": out(lab("A", 40), 7, "", false),
	"bad checksum at record 1": out(lab("B", 9, "A", 31), 7, "", false),
	"bad checksum at record 3": out(lab("B", 27, "A", 13), 7, "", false),
	// a record for page 0 or the lock-byte page ends playback
	"record for page 0 in the middle":                         out(lab("B", 18, "A", 22), 7, "", false),
	"record for the lock-byte page in the middle":             out(lab("B", 18, "A", 22), 1048578, "", false),
	"record above the initial size in the middle (db longer)": out(lab("B", 40), 7, "", false),
	// the last write of a page wins
	"duplicate page: junk then real": out(lab("B", 40), 7, "", false),
	"duplicate page: real then junk": out("", 7, malformed, false),
	// nRec
	"first header nRec 0, database longer than initial": out(lab("A", 40), 7, "", false),
	"nRec 0xffffffff":                      out(lab("B", 40), 7, "", false),
	"nRec 2 with more records present":     out(lab("B", 18, "A", 22), 7, "", false),
	"nRec larger than the records present": out(lab("B", 40), 7, "", false),
	// the engine adopts the journal's page size and rewrites the database with it
	"journal page size 2048 against a 1024 database": out(lab("B", 9, "A", 9, "-", 22), 14, malformed, false),
	// invalid header fields: nothing is applied (and nothing truncated)
	"header sector size 0":          out(lab("A", 40), 7, "", false),
	"header sector size 3":          out(lab("A", 40), 7, "", false),
	"header sector size 31":         out(lab("A", 40), 7, "", false),
	"header sector size 70000":      out(lab("A", 40), 7, "", false),
	"header sector size 4294967295": out(lab("A", 40), 7, "", false),
	// a page size field of 0 means the database's page size (it is applied)
	"header page size 0":     out(lab("B", 40), 7, "", false),
	"header page size 100":   out(lab("A", 40), 7, "", false),
	"header page size 70000": out(lab("A", 40), 7, "", false),
	// not hot, or hot with a bad magic
	"magic mismatch, first byte non-zero": out(lab("A", 40), 7, "", false),
	"first byte zero":                     out(lab("A", 40), 7, "", true),
	"zeroed header (persist)":             out(lab("A", 40), 7, "", true),
	"empty journal":                       out(lab("A", 40), 7, "", true),
	// the file is truncated to zero pages
	"initial pages 0": out("", 0, "SQL logic error: no such table: t (1)", false),
	// the journal supplies the pages the transaction cut off
	"journal supplies pages beyond the database end": out(lab("B", 40), 7, "", false),
	// later headers: their initial size is ignored, a zero count ends the journal
	"later segment with its own initial size": out(lab("B", 40), 7, "", false),
	"three segments":            out(lab("B", 40), 7, "", false),
	"later segment with nRec 0": out(lab("B", 18, "A", 22), 7, "", false),
	// a super-journal file that is absent skips the rollback; an empty file counts as absent
	"super-journal named, file absent":  out(lab("A", 40), 7, "", false),
	"super-journal named, file present": out(lab("B", 40), 7, "", false),
	// the rollback first, then the WAL on top (the first leaf holds rows 1..9)
	"hot journal and a WAL": out(lab("W", 5, "A", 4, "B", 31), 7, "", false),
	// a journal under 512 bytes is never played back (the first sector check)
	"journal of 32 bytes, sector 32, initial pages 3":                out(lab("A", 40), 7, "", false),
	"journal of 511 bytes, sector 32, initial pages 3":               out(lab("A", 40), 7, "", false),
	"journal of 512 bytes, sector 32, initial pages 3":               out("", 3, malformed, false),
	"initial size above the file, record for the last extra page":    out(lab("B", 40), 10, "", false),
	"initial size above the file, records for all extra pages":       out(lab("B", 40), 10, "", false),
	"later segment with its own nonce":                               out(lab("B", 40), 7, "", false),
	"super-journal name of 1040 bytes, file absent, nRec 0xffffffff": out(lab("A", 40), 7, "", false),
	"super-journal name of 1041 bytes, file absent, nRec 0xffffffff": out(lab("B", 40), 7, "", false),
}

// engineDB writes copies of db and journal (wal nil: none) and opens them in
// the engine; nothing is queried yet.
func (j *jfix) engineDB(t testing.TB, db, journal []byte) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "j.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if journal != nil {
		if err := os.WriteFile(p+"-journal", journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return openEngine(t, p)
}

// engineRows lets the engine roll back copies and returns the rows of t.
func (j *jfix) engineRows(t testing.TB, db, journal, _ []byte) [][]any {
	t.Helper()
	return engineQuery(t, j.engineDB(t, db, journal), "select rowid, a, b from t order by rowid")
}

// superNameMax is the longest super-journal name the engine honours (measured:
// 1040 bytes are looked up, 1041 make the engine ignore the trailer).
const superNameMax = 1040

// longSuperName is an absent path of n bytes.
func longSuperName(n int) string {
	const p = "/nonexistent/"
	return p + strings.Repeat("x", n-len(p))
}

// gapJournal is a journal whose initial size is 10 pages for a 7-page database,
// with records for the given extra pages and the changed leaves.
func gapJournal(extra []uint32) buildFn {
	return func(j *jfix, _ string) ([]byte, []byte, []byte) {
		jr := j.f.b.NewJournal(512, 0xdeadbeef, 10)
		for _, pg := range extra {
			jr.Record(pg, junkPage())
		}
		for _, pg := range j.changed {
			jr.Record(pg, bytes.Clone(j.s0.Page(pg)))
		}
		return j.dbAfter, jr.Bytes(), nil
	}
}
