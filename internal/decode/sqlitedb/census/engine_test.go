package census_test

// The engine oracle of the census. The modernc driver is imported in test files only and only
// ever opens COPIES the tests write to their own temporary directory.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
)

func engineOnCopy(t testing.TB, db, wal []byte) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	e.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func engineCount(t testing.TB, db, wal []byte, table string, want int64) {
	t.Helper()
	var n int64
	if err := engineOnCopy(t, db, wal).QueryRow("select count(*) from " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("the engine counts %d rows in %s, the test expects %d", n, table, want)
	}
}

// engineDB writes a database with the engine and returns its bytes.
func engineDB(t testing.TB, stmts ...string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e.db")
	e, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	e.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := e.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCensusObjectsMatchEngine(t *testing.T) {
	db := engineDB(t,
		"CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid TEXT, data TEXT)",
		"CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid TEXT UNIQUE)",
		"CREATE INDEX messages_jid ON messages (key_remote_jid)",
		"CREATE VIEW message_view AS SELECT * FROM messages",
		"CREATE TRIGGER messages_bd BEFORE DELETE ON messages BEGIN DELETE FROM chat WHERE _id = old._id; END",
	)
	rep, err := census.Take(t.Context(), files(db, nil), newBudget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := engineOnCopy(t, db, nil).Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var want []census.Object
	for rows.Next() {
		var o census.Object
		if err := rows.Scan(&o.Type, &o.Name, &o.TblName, &o.SQL); err != nil {
			t.Fatal(err)
		}
		want = append(want, o)
	}
	if len(want) < 6 {
		t.Fatalf("the engine lists %d objects", len(want))
	}
	if len(rep.Objects) != len(want) {
		t.Fatalf("census %d objects, engine %d", len(rep.Objects), len(want))
	}
	for i := range want {
		if rep.Objects[i] != want[i] {
			t.Errorf("object %d: census %+v, engine %+v", i, rep.Objects[i], want[i])
		}
	}
}

func TestCensusCountsMatchEngine(t *testing.T) {
	db := engineDB(t,
		"CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid)",
		"INSERT INTO messages (key_remote_jid) VALUES ('a@s.whatsapp.net'), ('-1'), (-1), (NULL), (x'2d31')",
	)
	rep, err := census.Take(t.Context(), files(db, nil), newBudget(), []census.CountSpec{{Table: "messages", Column: "key_remote_jid", Equals: "-1"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows, matched int64
	e := engineOnCopy(t, db, nil)
	if err := e.QueryRow("select count(*), sum(typeof(key_remote_jid)='text' AND key_remote_jid='-1') from messages").Scan(&rows, &matched); err != nil {
		t.Fatal(err)
	}
	c := rep.Counts[0]
	if rows != 5 || matched != 1 || c.Rows != rows || c.Matched != matched {
		t.Fatalf("census %+v, engine rows=%d matched=%d", c, rows, matched)
	}
}
