package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// idxWorkloads are the genuine shapes of index the recomputation must equal exactly: the five of the
// decoder tests, a doclist that carries a doclist index (also at a page size small enough for the
// index itself to need several levels), and the two ways a merge can stop half way.
func idxWorkloads() map[string]func(testing.TB, *sql.DB) {
	w := genuineFTSWorkloads()
	w["long doclist"] = longDoclistWorkload
	w["long doclist, huge poslist"] = func(t testing.TB, db *sql.DB) {
		probeExec(t, db, "BEGIN")
		for i := 1; i <= 6000; i++ {
			for _, table := range FTSTables() {
				body := fmt.Sprintf("common u%d", i%251)
				if i == 3000 { // a poslist of several pages: pages with neither a term nor a rowid inside a doclist index
					body = strings.Repeat("common ", 20000)
				}
				probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, "common", body)
			}
		}
		probeExec(t, db, "COMMIT")
	}
	w["long doclist, 64-byte pages"] = func(t testing.TB, db *sql.DB) {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('pgsz', 64)")
		}
		probeExec(t, db, "BEGIN")
		for i := 1; i <= 4000; i++ {
			for _, table := range FTSTables() {
				body := fmt.Sprintf("common u%d", i%251)
				if i == 2000 { // a poslist that fills whole pages: they hold no term and no rowid
					body = strings.Repeat("common ", 600)
				}
				probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, "common", body)
			}
		}
		probeExec(t, db, "COMMIT")
	}
	// automerge off, 40 small segments, then incremental merges of 2 pages that stop before they finish: the
	// inputs are trimmed and, once drained, empty (first page 0, last page 0)
	w["partial merges"] = func(t testing.TB, db *sql.DB) {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('automerge', 0)")
		}
		for i := 0; i < 40; i++ {
			fill(t, db, 1+i*30, 30, 25)
		}
		for step := 0; step < 7; step++ {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('merge', 2)")
			}
		}
	}
	// the merge writer, not the flush writer, builds these doclist indexes
	w["long doclist, merged"] = func(t testing.TB, db *sql.DB) {
		longDoclistWorkload(t, db)
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+") VALUES('optimize')")
		}
	}
	// five segments with doclist indexes, merged half way
	w["long doclist, partial merge"] = func(t testing.TB, db *sql.DB) {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('automerge', 0)")
		}
		for part := 0; part < 5; part++ {
			probeExec(t, db, "BEGIN")
			for i := part*6000 + 1; i <= (part+1)*6000; i++ {
				for _, table := range FTSTables() {
					probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, "common", fmt.Sprintf("common u%d", i%251))
				}
			}
			probeExec(t, db, "COMMIT")
		}
		for step := 0; step < 3; step++ {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('merge', 20)")
			}
		}
	}
	w["merge steps"] = func(t testing.TB, db *sql.DB) {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('automerge', 0)")
		}
		for i := 0; i < 12; i++ {
			fill(t, db, 1+i*100, 100, 30)
		}
		for step := 0; step < 30; step++ {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('merge', 1)")
			}
		}
	}
	w["automerge steps"] = func(t testing.TB, db *sql.DB) {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('automerge', 2)")
		}
		for i := 0; i < 45; i++ {
			fill(t, db, 1+i*40, 40, 30)
		}
	}
	return w
}

// runIdxCheck runs the check in a read transaction, as verify does: the check holds two cursors at once, which a
// database with one connection serves only inside a transaction.
func runIdxCheck(db *sql.DB, table string, report func(ftsIdxFinding)) (ftsIdxStats, error) {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return ftsIdxStats{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return checkFTSIndex(context.Background(), tx, table, report, nil)
}

func TestFTS5IdxRecomputeEqualsGenuine(t *testing.T) {
	check := func(t *testing.T, name string, db *sql.DB) ftsIdxStats {
		t.Helper()
		var total ftsIdxStats
		for _, table := range FTSTables() {
			st, err := runIdxCheck(db, table, func(f ftsIdxFinding) {
				t.Errorf("%s %s: %s", name, table, f.text(table))
			})
			if err != nil {
				t.Fatalf("%s %s: %v", name, table, err)
			}
			var idxRows, dlidx int
			if err := db.QueryRow("SELECT count(*) FROM " + table + "_idx").Scan(&idxRows); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM "+table+"_data WHERE id >= ? AND (id >> 36) & 1 = 1", 1<<ftsSegidShift).Scan(&dlidx); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
			// the rows of a drained segment are not compared
			for _, seg := range ftsStructureOf(t, db, table) {
				if seg.First != 0 || seg.Last != 0 {
					continue
				}
				var ni, nd int
				if err := db.QueryRow("SELECT count(*) FROM "+table+"_idx WHERE segid = ?", seg.ID).Scan(&ni); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
				if err := db.QueryRow("SELECT count(*) FROM "+table+"_data WHERE id >= ? AND id < ? AND (id >> 36) & 1 = 1", seg.ID<<ftsSegidShift, (seg.ID+1)<<ftsSegidShift).Scan(&nd); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
				idxRows -= ni
				dlidx -= nd
			}
			if int64(idxRows) != st.IdxRows+st.StaleIdxRows || idxRows == 0 {
				t.Errorf("%s %s: %d _idx rows stored, %d matched and %d stale", name, table, idxRows, st.IdxRows, st.StaleIdxRows)
			}
			if dlidx != st.DlidxRows+st.StaleDlidxRows {
				t.Errorf("%s %s: %d doclist index rows stored, %d matched and %d stale", name, table, dlidx, st.DlidxRows, st.StaleDlidxRows)
			}
			t.Logf("%s %s: %d segments (%d trimmed, %d empty), %d pages, %d _idx rows (%d stale), %d doclist index rows (%d stale), %d whole-term keys",
				name, table, st.Segments, st.Trimmed, st.Empty, st.Pages, st.IdxRows, st.StaleIdxRows, st.DlidxRows, st.StaleDlidxRows, st.FullKeys)
			total.Trimmed += st.Trimmed
			total.Empty += st.Empty
			total.DlidxRows += st.DlidxRows
			total.FullKeys += st.FullKeys
		}
		return total
	}
	for name, load := range idxWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			st := check(t, name, db)
			switch name {
			case "long doclist", "long doclist, merged", "long doclist, huge poslist", "long doclist, 64-byte pages":
				if st.DlidxRows < 2 {
					t.Errorf("%s built %d doclist index rows: the workload does not test them", name, st.DlidxRows)
				}
			case "partial merges":
				if st.Trimmed == 0 || st.Empty == 0 {
					t.Errorf("%s built %d trimmed and %d empty segments: the workload does not test them", name, st.Trimmed, st.Empty)
				}
			}
		})
	}
	t.Run("ReindexText over 6000 records", func(t *testing.T) {
		c := indexedTextCase(t, 6000)
		check(t, "reindex", c.store.db)
	})
}

// a doclist index of several levels exists in the 64-byte workload: the recomputation transcribes the
// growth of the levels, and a flipped byte anywhere in them is a difference
func TestFTS5IdxMultiLevelDlidxIsExercised(t *testing.T) {
	db := genuineFTSWorkload(t, "long doclist, 64-byte pages", idxWorkloads()["long doclist, 64-byte pages"])
	for _, table := range FTSTables() {
		var maxLevel int
		if err := db.QueryRow("SELECT max((id >> 31) & 31) FROM "+table+"_data WHERE id >= ? AND (id >> 36) & 1 = 1", 1<<ftsSegidShift).Scan(&maxLevel); err != nil { //nolint:gosec // a constant table name in a test
			t.Fatal(err)
		}
		if maxLevel < 1 {
			t.Errorf("%s: the doclist index has only level %d", table, maxLevel)
		}
	}
}

// every byte of every doclist index row, changed, is a difference
func TestFTS5IdxDlidxByteTamperIsFound(t *testing.T) {
	db := genuineFTSWorkload(t, "long doclist, 64-byte pages", idxWorkloads()["long doclist, 64-byte pages"])
	table := FTSWordTable
	rows, err := db.Query("SELECT id, block FROM "+table+"_data WHERE id >= ? AND (id >> 36) & 1 = 1 ORDER BY id", 1<<ftsSegidShift) //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id    int64
		block []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.block); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	_ = rows.Close()
	if len(all) < 3 {
		t.Fatalf("%d doclist index rows", len(all))
	}
	// one row per level plus the first and the last: every byte of them
	for _, r := range []row{all[0], all[len(all)/2], all[len(all)-1]} {
		for i := range r.block {
			bad := append([]byte{}, r.block...)
			bad[i] ^= 0x01
			probeExec(t, db, "UPDATE "+table+"_data SET block = ? WHERE id = ?", bad, r.id)
			n := 0
			if _, err := runIdxCheck(db, table, func(ftsIdxFinding) { n++ }); err != nil {
				t.Fatal(err)
			}
			probeExec(t, db, "UPDATE "+table+"_data SET block = ? WHERE id = ?", r.block, r.id)
			if n == 0 {
				t.Errorf("row %d byte %d: a flipped bit is not found", r.id, i)
			}
		}
	}
}

// the constants the recomputation uses are the ones of the pinned SQLite
func TestFTS5IdxConstantsArePinned(t *testing.T) {
	if ftsMinDlidxSize != sqlite3.FTS5_MIN_DLIDX_SIZE || ftsDefaultPgsz != sqlite3.FTS5_DEFAULT_PAGE_SIZE {
		t.Fatalf("pinned constants %d %d, the driver has %d %d", ftsMinDlidxSize, ftsDefaultPgsz, sqlite3.FTS5_MIN_DLIDX_SIZE, sqlite3.FTS5_DEFAULT_PAGE_SIZE)
	}
	if ftsDlidxFlag != 1<<(sqlite3.FTS5_DATA_PAGE_B+sqlite3.FTS5_DATA_HEIGHT_B) || ftsSegidShift != sqlite3.FTS5_DATA_PAGE_B+sqlite3.FTS5_DATA_HEIGHT_B+sqlite3.FTS5_DATA_DLI_B {
		t.Fatal("the row id layout differs from the driver")
	}
}

// the varint encoder writes what SQLite reads back, canonically
func TestFTSAppendVarintRoundTrips(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<56 - 1, 1 << 56, 1<<63 + 5, math.MaxUint64} {
		b := ftsAppendVarint(nil, v)
		got, n := ftsVarint(b)
		if n != len(b) || got != v {
			t.Errorf("%d: wrote %x, read %d in %d bytes", v, b, got, n)
		}
	}
}

// withoutIntegrity drops the lines of PRAGMA integrity_check (P10), which also notices some of these tampers; the
// lines of P11 are the ones under test.
func withoutIntegrity(ps []string) []string {
	var out []string
	for _, p := range ps {
		if !strings.Contains(p, "integrity_check") {
			out = append(out, p)
		}
	}
	return out
}

func tamperText(i int) (string, string) {
	return fmt.Sprintf("common s%d", i), fmt.Sprintf("common b%d tail%d", i, i%97)
}

func TestVerifyFTSDlidxTamperIsAProblem(t *testing.T) {
	c, rec := caseWithArtifact(t)
	insertTextRecords(t, c, rec.ID, 6000, tamperText)
	if _, err := c.ReindexText(context.Background(), ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if p := ftsProblems(mustVerify(t, c)); len(p) != 0 {
		t.Fatalf("the clean long-doclist case has full-text problems: %q", p)
	}
	var id int64
	var block []byte
	if err := c.store.db.QueryRow("SELECT id, block FROM records_fts_data WHERE id >= ? AND (id >> 36) & 1 = 1 ORDER BY id LIMIT 1", 1<<ftsSegidShift).Scan(&id, &block); err != nil {
		t.Fatalf("no doclist index in the case: %v", err)
	}
	block[len(block)-1] ^= 0x01
	if _, err := c.store.db.Exec("UPDATE records_fts_data SET block = ? WHERE id = ?", block, id); err != nil {
		t.Fatal(err)
	}
	fts := withoutIntegrity(ftsProblems(mustVerify(t, c)))
	n := 0
	for _, p := range fts {
		if strings.Contains(p, "the doclist index of term") {
			n++
		}
		for _, bad := range []string{"the index lacks", "is invented", "totals differ", "size entry"} {
			if strings.Contains(p, bad) {
				t.Errorf("a content problem beside the doclist index one: %s", p)
			}
		}
	}
	if n != 1 || len(fts) != 1 {
		t.Fatalf("%d doclist index lines among %q", n, fts)
	}
}

func TestVerifyFTSFooterTamperIsAProblem(t *testing.T) {
	c := indexedTextCase(t, 300)
	var id int64
	var block []byte
	rows, err := c.store.db.Query("SELECT id, block FROM records_fts_data WHERE id >= ? AND id < ? ORDER BY id", 1<<ftsSegidShift, 1<<ftsSegidShift+1<<31)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var i int64
		var b []byte
		if err := rows.Scan(&i, &b); err != nil {
			t.Fatal(err)
		}
		szLeaf := int(b[2])<<8 | int(b[3])
		if len(b)-szLeaf >= 2 && id == 0 {
			id, block = i, b
		}
	}
	_ = rows.Close()
	if id == 0 {
		t.Fatal("no leaf with two terms")
	}
	szLeaf := int(block[2])<<8 | int(block[3])
	// the first footer varint is the offset of the first term; the second is the distance to the next
	block[szLeaf+1]++
	if _, err := c.store.db.Exec("UPDATE records_fts_data SET block = ? WHERE id = ?", block, id); err != nil {
		t.Fatal(err)
	}
	fts := withoutIntegrity(ftsProblems(mustVerify(t, c)))
	found := false
	for _, p := range fts {
		found = found || strings.Contains(p, "is not a valid leaf: footer mismatch")
	}
	if !found {
		t.Fatalf("no invalid-leaf line: %q", fts)
	}
}

func TestVerifyFTSIdxMissingRowConfirmsLostHit(t *testing.T) {
	c := indexedTextCase(t, 3000)
	if p := ftsProblems(mustVerify(t, c)); len(p) != 0 {
		t.Fatalf("the clean case has full-text problems: %q", p)
	}
	var seg int64
	var term []byte
	if err := c.store.db.QueryRow("SELECT segid, term FROM records_fts_idx WHERE term <> x'' ORDER BY segid, term LIMIT 1 OFFSET 7").Scan(&seg, &term); err != nil {
		t.Fatal(err)
	}
	if res, err := c.store.db.Exec("DELETE FROM records_fts_idx WHERE segid = ? AND term = ?", seg, term); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%d rows deleted", n)
	}
	fts := withoutIntegrity(ftsProblems(mustVerify(t, c)))
	missing, lost := 0, 0
	for _, p := range fts {
		if strings.Contains(p, "records_fts_idx: segment") && strings.Contains(p, "is missing (recomputed from the leaf pages)") {
			missing++
		}
		if strings.Contains(p, `records_fts: a search for "`) && strings.Contains(p, "does not find record") &&
			strings.Contains(p, "(a search hit is lost: the index structure no longer reaches it)") {
			lost++
		}
		for _, bad := range []string{"the index lacks", "is invented", "totals differ", "size entry"} {
			if strings.Contains(p, bad) {
				t.Errorf("a content problem beside the index one: %s", p)
			}
		}
	}
	if missing != 1 || lost != 1 || len(fts) != 2 {
		t.Fatalf("%d missing-row lines, %d lost-hit lines among %q", missing, lost, fts)
	}
}

func TestVerifyCancelledDuringTheIndexPass(t *testing.T) {
	c := indexedTextCase(t, 3000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "fts-data" {
			seen++
			cancel()
		}
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the index pass never reported a chunk of pages")
	}
	if rep.OK() {
		t.Fatal("a cancelled verify must not be OK")
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "context canceled") || strings.Contains(p, "unreadable") || strings.Contains(p, "could not") {
			t.Fatalf("cancel noise in the report: %q", p)
		}
	}
}

// every kind of wrong row is a finding of its own kind: the recomputation does not rely on one tamper
func TestFTS5IdxTamperIsFound(t *testing.T) {
	const table = FTSWordTable
	type tamper struct {
		name     string
		workload string
		kind     ftsIdxKind
		do       func(t *testing.T, db *sql.DB, seg ftsSegment)
	}
	nth := func(t *testing.T, db *sql.DB, seg ftsSegment, off int) (term []byte, pgno int64) {
		t.Helper()
		if err := db.QueryRow("SELECT term, pgno FROM "+table+"_idx WHERE segid = ? AND term <> x'' ORDER BY term LIMIT 1 OFFSET ?", seg.ID, off).Scan(&term, &pgno); err != nil {
			t.Fatal(err)
		}
		return term, pgno
	}
	dlidxRow := func(t *testing.T, db *sql.DB) (id int64, block []byte) {
		t.Helper()
		if err := db.QueryRow("SELECT id, block FROM "+table+"_data WHERE id >= ? AND (id >> 36) & 1 = 1 ORDER BY id LIMIT 1", 1<<ftsSegidShift).Scan(&id, &block); err != nil {
			t.Fatal(err)
		}
		return id, block
	}
	tampers := []tamper{
		{"first row deleted", "after optimize", ftsIdxMissing, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "DELETE FROM "+table+"_idx WHERE segid = ? AND term = x''", seg.ID)
		}},
		{"last row deleted", "after optimize", ftsIdxMissing, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "DELETE FROM "+table+"_idx WHERE segid = ? AND term = (SELECT max(term) FROM "+table+"_idx WHERE segid = ?)", seg.ID, seg.ID)
		}},
		{"middle row deleted", "after optimize", ftsIdxMissing, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			term, _ := nth(t, db, seg, 20)
			probeExec(t, db, "DELETE FROM "+table+"_idx WHERE segid = ? AND term = ?", seg.ID, term)
		}},
		{"page moved on", "after optimize", ftsIdxWrong, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			term, _ := nth(t, db, seg, 20)
			probeExec(t, db, "UPDATE "+table+"_idx SET pgno = pgno + 2 WHERE segid = ? AND term = ?", seg.ID, term)
		}},
		{"doclist index flag flipped", "after optimize", ftsIdxWrong, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			term, _ := nth(t, db, seg, 20)
			probeExec(t, db, "UPDATE "+table+"_idx SET pgno = pgno + 1 - 2 * (pgno & 1) WHERE segid = ? AND term = ?", seg.ID, term)
		}},
		{"key lengthened", "after optimize", ftsIdxMissing, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			term, _ := nth(t, db, seg, 20)
			probeExec(t, db, "UPDATE "+table+"_idx SET term = term || x'7e7e' WHERE segid = ? AND term = ?", seg.ID, term)
		}},
		{"row of a page past the last", "after optimize", ftsIdxNoLeaf, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "INSERT INTO "+table+"_idx(segid, term, pgno) VALUES (?, x'307a7a7a', ?)", seg.ID, (seg.Last+5)<<1)
		}},
		{"first row key changed", "after optimize", ftsIdxMissing, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "UPDATE "+table+"_idx SET term = x'3001' WHERE segid = ? AND term = x''", seg.ID)
		}},
		{"row of page 0", "after optimize", ftsIdxNoLeaf, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "INSERT INTO "+table+"_idx(segid, term, pgno) VALUES (?, x'3001', 0)", seg.ID)
		}},
		{"second row for a page", "after optimize", ftsIdxWrong, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "INSERT INTO "+table+"_idx(segid, term, pgno) VALUES (?, x'3001', 6)", seg.ID)
		}},
		{"key of another storage class", "after optimize", ftsIdxBadClass, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			term, _ := nth(t, db, seg, 20)
			probeExec(t, db, "UPDATE "+table+"_idx SET term = CAST(term AS TEXT) WHERE segid = ? AND term = ?", seg.ID, term)
		}},
		{"leaf page deleted", "after optimize", ftsLeafBad, func(t *testing.T, db *sql.DB, seg ftsSegment) {
			probeExec(t, db, "DELETE FROM "+table+"_data WHERE id = ?", seg.ID<<ftsSegidShift+7)
		}},
		{"doclist index row deleted", "long doclist", ftsDlidxDiffers, func(t *testing.T, db *sql.DB, _ ftsSegment) {
			id, _ := dlidxRow(t, db)
			probeExec(t, db, "DELETE FROM "+table+"_data WHERE id = ?", id)
		}},
		{"doclist index row without a doclist", "long doclist", ftsDlidxExtra, func(t *testing.T, db *sql.DB, _ ftsSegment) {
			id, block := dlidxRow(t, db)
			probeExec(t, db, "INSERT INTO "+table+"_data(id, block) VALUES (?, ?)", id+1000, block)
		}},
		{"doclist index flag cleared in _idx", "long doclist", ftsIdxWrong, func(t *testing.T, db *sql.DB, _ ftsSegment) {
			probeExec(t, db, "UPDATE "+table+"_idx SET pgno = pgno & ~1 WHERE pgno & 1 = 1")
		}},
	}
	for _, tp := range tampers {
		t.Run(tp.name, func(t *testing.T) {
			db := genuineFTSWorkload(t, tp.workload, idxWorkloads()[tp.workload])
			seg := ftsStructureOf(t, db, table)[0]
			if tp.workload == "long doclist" { // the segment that holds the doclist index
				for _, s := range ftsStructureOf(t, db, table) {
					var n int
					if err := db.QueryRow("SELECT count(*) FROM "+table+"_data WHERE id >= ? AND id < ? AND (id >> 36) & 1 = 1", s.ID<<ftsSegidShift, (s.ID+1)<<ftsSegidShift).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n > 0 {
						seg = s
					}
				}
			}
			var kinds []ftsIdxKind
			if _, err := runIdxCheck(db, table, func(f ftsIdxFinding) { kinds = append(kinds, f.Kind) }); err != nil {
				t.Fatal(err)
			}
			if len(kinds) != 0 {
				t.Fatalf("the genuine index has findings before the tamper: %v", kinds)
			}
			tp.do(t, db, seg)
			kinds = nil
			if _, err := runIdxCheck(db, table, func(f ftsIdxFinding) {
				kinds = append(kinds, f.Kind)
				_ = f.text(table)
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(kinds, tp.kind) {
				t.Fatalf("no finding of kind %d among %v", tp.kind, kinds)
			}
		})
	}
}

// the search that confirms a difference must find what a genuine index holds: for every recomputed _idx row the
// first term of its page, restricted to the first record of its doclist, is found, so a lost-hit line is never a
// false claim
func TestFTS5IdxConfirmingSearchFindsGenuineHits(t *testing.T) {
	for _, name := range []string{"long doclist", "long doclist, 64-byte pages", "long doclist, partial merge", "partial merges", "one huge body"} {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, idxWorkloads()[name])
			for _, table := range FTSTables() {
				var rows []ftsIdxRow
				tx, err := db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				_, err = checkFTSIndexRows(context.Background(), tx, table, func(f ftsIdxFinding) { t.Errorf("%s: %s", table, f.text(table)) }, nil,
					func(r ftsIdxRow) { rows = append(rows, r) })
				_ = tx.Rollback()
				if err != nil {
					t.Fatal(err)
				}
				withRowid := 0
				for _, r := range rows {
					if !r.HasRowid {
						continue
					}
					withRowid++
					var one int
					err := db.QueryRow("SELECT 1 FROM "+table+" WHERE "+table+" MATCH ? AND rowid = ? LIMIT 1", ftsQuote(ftsLostHitTerm(r.FirstTerm)), r.FirstRowid).Scan(&one) //nolint:gosec // a constant table name in a test
					if err != nil {
						t.Fatalf("%s: segment %d page %d: a search for %q and record %d finds nothing: %v", table, r.Segid, r.Pgno>>1, ftsShowTerm(r.FirstTerm), r.FirstRowid, err)
					}
				}
				if withRowid == 0 {
					t.Errorf("%s: no recomputed row names a record", table)
				}
			}
		})
	}
}
