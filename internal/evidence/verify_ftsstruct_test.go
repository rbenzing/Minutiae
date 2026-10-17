package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// genuineFTS builds both FTS tables in a scratch file database with the real DDL and returns it.
func genuineFTS(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "g.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range FTSTables() {
		ddl, ok := FTSTableDDL(table)
		if !ok {
			t.Fatalf("no DDL for %s", table)
		}
		probeExec(t, db, ddl)
	}
	return db
}

func fill(t *testing.T, db *sql.DB, from, n, words int) {
	t.Helper()
	for _, table := range FTSTables() {
		for i := from; i < from+n; i++ {
			var b strings.Builder
			for w := 0; w < words; w++ {
				fmt.Fprintf(&b, "w%d_%d ", (i*7+w)%997, i%13)
			}
			probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, fmt.Sprintf("s%d", i), b.String())
		}
	}
}

// accounted requires the decoded segment set to equal the segment ids of the data rows exactly, and
// the idx rows to name only decoded segments.
func accounted(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var structure []byte
	if err := db.QueryRow("SELECT block FROM "+table+"_data WHERE id = ?", ftsStructureRowID).Scan(&structure); err != nil {
		t.Fatal(err)
	}
	segs, err := decodeFTSStructure(structure)
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	rows, err := db.Query("SELECT id FROM "+table+"_data WHERE id NOT IN (?, ?)", ftsAveragesRowID, ftsStructureRowID) //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id < 1<<ftsSegidShift {
			t.Fatalf("%s: genuine data row %d is below the segment range", table, id)
		}
		seen[id>>ftsSegidShift] = struct{}{}
	}
	_ = rows.Close()
	if len(seen) != len(segs) {
		t.Fatalf("%s: data rows hold %d segments, the structure names %d", table, len(seen), len(segs))
	}
	for s := range seen {
		if _, ok := segs[s]; !ok {
			t.Fatalf("%s: data row segment %d is not in the structure", table, s)
		}
	}
	ir, err := db.Query("SELECT DISTINCT segid FROM " + table + "_idx") //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	for ir.Next() {
		var s int64
		if err := ir.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if _, ok := segs[s]; !ok {
			t.Fatalf("%s: idx segment %d is not in the structure", table, s)
		}
	}
	_ = ir.Close()
}

func TestFTSStructureAccountsForEveryGenuineRow(t *testing.T) {
	workloads := map[string]func(t *testing.T, db *sql.DB){
		"one document": func(t *testing.T, db *sql.DB) { fill(t, db, 1, 1, 5) },
		"many small inserts, merges": func(t *testing.T, db *sql.DB) {
			for i := 0; i < 60; i++ {
				fill(t, db, 1+i*50, 50, 40)
			}
		},
		"after optimize": func(t *testing.T, db *sql.DB) {
			for i := 0; i < 20; i++ {
				fill(t, db, 1+i*50, 50, 40)
			}
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+") VALUES('optimize')")
			}
		},
		"one huge body": func(t *testing.T, db *sql.DB) {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (1, 's', ?)", strings.Repeat("alpha beta gamma delta ", 9000))
			}
		},
		"after delete-all and refill": func(t *testing.T, db *sql.DB) {
			fill(t, db, 1, 300, 20)
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+") VALUES('delete-all')")
			}
			fill(t, db, 1000, 100, 20)
		},
	}
	for name, load := range workloads {
		t.Run(name, func(t *testing.T) {
			db := genuineFTS(t)
			load(t, db)
			for _, table := range FTSTables() {
				accounted(t, db, table)
			}
		})
	}
}

func TestFTSStructureRejectsHostileRecords(t *testing.T) {
	db := genuineFTS(t)
	fill(t, db, 1, 200, 20)
	var good []byte
	if err := db.QueryRow("SELECT block FROM records_fts_data WHERE id = ?", ftsStructureRowID).Scan(&good); err != nil {
		t.Fatal(err)
	}
	huge := append(append([]byte{0, 0, 0, 0}, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f), 0x01, 0x00)
	for name, b := range map[string][]byte{
		"empty":                      nil,
		"three bytes":                {1, 2, 3},
		"cookie only":                {0, 0, 0, 0},
		"huge level count":           huge,
		"truncated":                  good[:len(good)-1],
		"trailing bytes":             append(append([]byte{}, good...), 0x01),
		"more segments than fit":     append([]byte{0, 0, 0, 0, 0x01, 0x7f, 0x00}, 0x01),
		"segment total differs":      append([]byte{0, 0, 0, 0, 0x01, 0x02, 0x00, 0x00, 0x01, 0x05, 0x01, 0x01}, 0),
		"last page before the first": {0, 0, 0, 0, 0x01, 0x01, 0x00, 0x00, 0x01, 0x01, 0x05, 0x02},
		"merging more than present":  {0, 0, 0, 0, 0x01, 0x01, 0x00, 0x02, 0x01, 0x01, 0x01, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			if segs, err := decodeFTSStructure(b); err == nil {
				t.Fatalf("accepted a malformed record: %v", segs)
			}
		})
	}
	if _, err := decodeFTSStructure(good); err != nil {
		t.Fatalf("the genuine record is refused: %v", err)
	}
}

// A hostile record cannot make the decoder allocate more than it has bytes: the map starts small.
func TestFTSStructureDoesNotAllocateFromADeclaredCount(t *testing.T) {
	b := append([]byte{0, 0, 0, 0}, 0x01, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00) // 1 level, 2^35-1 segments
	allocs := testing.AllocsPerRun(20, func() { _, _ = decodeFTSStructure(b) })
	if allocs > 20 {
		t.Fatalf("%v allocations for a 7-byte hostile record", allocs)
	}
}

// The structure check on a cancelled context adds no problem of its own: the cancellation is what the
// caller reports, and a "could not be checked" line beside it is noise. A read error on a live context
// is still reported.
func TestFTSStructureCheckIsQuietWhenCancelled(t *testing.T) {
	c := indexedTextCase(t, 50)
	run := func(ctx context.Context) []string {
		rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
		r := &ftsVerify{c: c, ctx: ctx, ps: &problemSet{rep: rep, counts: map[string]int{}}, rep: rep, problems: map[string]int{}, listed: map[string]map[int64]bool{}}
		r.checkUnreferenced(FTSWordTable)
		return rep.Problems
	}
	if p := run(context.Background()); len(p) != 0 {
		t.Fatalf("a live check of a clean index reports %q", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p := run(ctx); len(p) != 0 {
		t.Fatalf("a cancelled check reports %q", p)
	}
}
