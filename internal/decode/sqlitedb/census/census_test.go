package census_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

type testBudget struct{ limit, used int64 }

func (b *testBudget) Alloc(n int64) error {
	if n < 0 || b.used+n > b.limit {
		return fmt.Errorf("%w: %d bytes requested", parse.ErrBudget, n)
	}
	b.used += n
	return nil
}

func (b *testBudget) Free(n int64) { b.used = max(0, b.used-n) }

func newBudget() *testBudget { return &testBudget{limit: 1 << 40} }

func files(db, wal []byte) sqlitedb.Files {
	f := sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}
	if wal != nil {
		f.WAL, f.WALSize = bytes.NewReader(wal), int64(len(wal))
	}
	return f
}

const msgDDL = "CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid TEXT)"

// walScenario: the database file holds `messages` with 2 rows; a committed WAL transaction adds
// 3 rows and a table `late` that exists only in the WAL; an uncommitted tail adds 4 more rows.
func walScenario(t testing.TB, uncommitted bool) (db, wal []byte) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	m := b.CreateTable("messages", msgDDL)
	m.Insert(1, nil, "a")
	m.Insert(2, nil, "-1")
	db = b.Bytes()
	snap := b.Snapshot()
	w := b.NewWAL(false, 0x11, 0x22, 0)
	for i := int64(3); i <= 5; i++ {
		m.Insert(i, nil, "x")
	}
	l := b.CreateTable("late", "CREATE TABLE late (_id INTEGER PRIMARY KEY, v TEXT)")
	l.Insert(1, nil, "v")
	b.CommitTo(w, snap)
	if uncommitted {
		snap2 := b.Snapshot()
		for i := int64(6); i <= 9; i++ {
			m.Insert(i, nil, "u")
		}
		now := b.Snapshot()
		for p := uint32(1); p <= now.Pages(); p++ {
			if p > snap2.Pages() || !bytes.Equal(now.Page(p), snap2.Page(p)) {
				w.Frame(p, now.Page(p), 0)
			}
		}
	}
	return db, w.Bytes()
}

func TestCensusSeesWALOnlyTable(t *testing.T) {
	db, wal := walScenario(t, false)
	rep, err := census.Take(t.Context(), files(db, wal), newBudget(), []census.CountSpec{{Table: "messages"}, {Table: "late"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Counts[1].Present || rep.Counts[1].Rows != 1 {
		t.Fatalf("late = %+v", rep.Counts[1])
	}
	if rep.Counts[0].Rows != 5 {
		t.Fatalf("messages = %+v, want 5 rows", rep.Counts[0])
	}
	engineCount(t, db, wal, "messages", 5)
	engineCount(t, db, wal, "late", 1)
}

func TestCensusNeverCountsUncommittedFrames(t *testing.T) {
	db, wal := walScenario(t, true)
	rep, err := census.Take(t.Context(), files(db, wal), newBudget(), []census.CountSpec{{Table: "messages"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts[0].Rows != 5 {
		t.Fatalf("messages = %+v, want the 5 committed rows only", rep.Counts[0])
	}
	engineCount(t, db, wal, "messages", 5)
}

func TestCensusUnreadableTableIsStated(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	m := b.CreateTable("messages", msgDDL)
	for i := int64(1); i <= 3; i++ {
		m.Insert(i, nil, "a")
	}
	pg := b.PageBytes(m.Root())
	for i := range pg {
		pg[i] = 0
	}
	rep, err := census.Take(t.Context(), files(b.Bytes(), nil), newBudget(), []census.CountSpec{{Table: "messages"}})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	c := rep.Counts[0]
	if !c.Present || c.Unreadable == "" {
		t.Fatalf("count = %+v, want present and unreadable", c)
	}
}

func TestCensusAbsentTableIsNotPresent(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("messages", msgDDL)
	rep, err := census.Take(t.Context(), files(b.Bytes(), nil), newBudget(), []census.CountSpec{{Table: "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := rep.Counts[0]; c.Present || c.Rows != 0 || c.Unreadable != "" {
		t.Fatalf("count = %+v", c)
	}
}

func TestCensusSQLIsCappedAndFlagged(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("messages", msgDDL)
	long := "CREATE VIEW message_view AS SELECT 1 FROM messages /*" + strings.Repeat("x", 70<<10) + "*/"
	b.AddSchemaRow("view", "message_view", "message_view", 0, long)
	rep, err := census.Take(t.Context(), files(b.Bytes(), nil), newBudget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range rep.Objects {
		if o.Name == "message_view" {
			found = true
			if len(o.SQL) != census.MaxSQL || !o.SQLTruncated {
				t.Fatalf("len %d truncated %v", len(o.SQL), o.SQLTruncated)
			}
		}
	}
	if !found {
		t.Fatal("the view is missing")
	}
}

func TestCensusRefusesNonSQLiteAndEncrypted(t *testing.T) {
	for name, data := range map[string][]byte{"zero": make([]byte, 4096), "text": bytes.Repeat([]byte("hello world "), 400), "empty": {}} {
		_, err := census.Take(t.Context(), files(data, nil), newBudget(), nil)
		if !errors.Is(err, sqlitedb.ErrNotSQLite) || errors.Is(err, sqlitedb.ErrEncrypted) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var enc []byte
	for i := uint64(0); len(enc) < 64<<10; i++ {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], i)
		h := sha256.Sum256(c[:])
		enc = append(enc, h[:]...)
	}
	_, err := census.Take(t.Context(), files(enc, nil), newBudget(), nil)
	if !errors.Is(err, sqlitedb.ErrEncrypted) || !errors.Is(err, sqlitedb.ErrNotSQLite) {
		t.Errorf("encrypted: err = %v", err)
	}
}

func TestCensusFreesItsBudget(t *testing.T) {
	db, wal := walScenario(t, false)
	b := newBudget()
	if _, err := census.Take(t.Context(), files(db, wal), b, []census.CountSpec{{Table: "messages", Column: "key_remote_jid", Equals: "-1"}}); err != nil {
		t.Fatal(err)
	}
	if b.used != 0 {
		t.Fatalf("%d bytes still charged", b.used)
	}
	small := &testBudget{limit: 100}
	if _, err := census.Take(t.Context(), files(db, wal), small, nil); !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("err = %v, want a budget error", err)
	}
	if small.used != 0 {
		t.Fatalf("%d bytes still charged after a refusal", small.used)
	}
}

func TestCensusRefusesTooManySpecs(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	_, err := census.Take(t.Context(), files(b.Bytes(), nil), newBudget(), make([]census.CountSpec, census.MaxSpecs+1))
	if !errors.Is(err, census.ErrTooManySpecs) {
		t.Fatalf("err = %v", err)
	}
}
