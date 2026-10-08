package evidence

// Probes of the SQLite FTS5 behaviours the full-text design (plan 5B) relies on.
// Each test is a permanent guard run on a scratch contentless table: if a driver
// or SQLite upgrade changes one of these behaviours, it fails loudly instead of
// silently weakening the search index or its verification.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	probeWordDDL = `CREATE VIRTUAL TABLE records_fts USING fts5(summary, body, content='', tokenize='unicode61 remove_diacritics 2')`
	probeSubDDL  = `CREATE VIRTUAL TABLE records_fts_sub USING fts5(summary, body, content='', tokenize='trigram case_sensitive 0 remove_diacritics 1')`
)

// probeTable is one FTS table and the vocab table that exposes its postings.
type probeTable struct{ name, vocab, ddl string }

var probeTables = []probeTable{
	{"records_fts", "records_fts_v", probeWordDDL},
	{"records_fts_sub", "records_fts_sub_v", probeSubDDL},
}

func probeExec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// probeDB opens a file database in t.TempDir() holding the given tables and
// their vocab tables.
func probeDB(t *testing.T, tables ...probeTable) *sql.DB {
	t.Helper()
	db := rawDB(t, filepath.Join(t.TempDir(), "probe.db"))
	probeCreate(t, db, tables...)
	return db
}

func probeCreate(t testing.TB, db *sql.DB, tables ...probeTable) {
	t.Helper()
	for _, tb := range tables {
		probeExec(t, db, tb.ddl)
		probeExec(t, db, `CREATE VIRTUAL TABLE `+tb.vocab+` USING fts5vocab(`+tb.name+`, 'instance')`)
	}
}

// probeDump runs q and renders every row, one string per row; blobs are hex.
func probeDump(t testing.TB, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				parts[i] = fmt.Sprintf("blob:%x", x)
			case string:
				parts[i] = fmt.Sprintf("text:%q", x)
			default:
				parts[i] = fmt.Sprintf("%T:%v", x, x)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func queryInts(db *sql.DB, q string, args ...any) ([]int64, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func probeInts(t testing.TB, db *sql.DB, q string, args ...any) []int64 {
	t.Helper()
	out, err := queryInts(db, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func probeMatch(t testing.TB, db *sql.DB, table, expr string) []int64 {
	t.Helper()
	return probeInts(t, db, `SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? ORDER BY rowid`, expr)
}

// probeQuote makes s one FTS5 string.
func probeQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// probeCorpus is a deterministic corpus of n documents with several distinct
// terms each (enough that an index holds many leaf pages).
func probeCorpus(n int) (summary, body []string) {
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a deterministic test corpus, not security
	word := func() string { return fmt.Sprintf("w%06d", rng.IntN(60000)) }
	for i := 0; i < n; i++ {
		summary = append(summary, word()+" "+word()+" "+word())
		body = append(body, word()+" "+word()+" "+word())
	}
	return summary, body
}

func probeInsertCorpus(t testing.TB, db *sql.DB, table string, summary, body []string, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		probeExec(t, db, `INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, i+1, summary[i], body[i])
	}
}

// vocabRow is one (term, doc, column, offset) entry of an fts5vocab instance table.
type vocabRow struct {
	term     string
	doc      int64
	col      string
	colIndex int
	offset   int64
}

func probeVocab(t testing.TB, db *sql.DB, vocab string) []vocabRow {
	t.Helper()
	rows, err := db.Query(`SELECT term, doc, col, offset FROM ` + vocab) //nolint:gosec // a constant table name in a test; natural order: no ORDER BY
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []vocabRow
	for rows.Next() {
		var r vocabRow
		if err := rows.Scan(&r.term, &r.doc, &r.col, &r.offset); err != nil {
			t.Fatal(err)
		}
		switch r.col {
		case "summary":
			r.colIndex = 0
		case "body":
			r.colIndex = 1
		default:
			t.Fatalf("vocab column %q is neither summary nor body", r.col)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFTS5VocabOnContentlessBothTokenizers: fts5vocab(.., 'instance') works on
// contentless tables of both tokenizers and returns (term, doc, col, offset),
// col being the column NAME.
func TestFTS5VocabOnContentlessBothTokenizers(t *testing.T) {
	db := probeDB(t, probeTables...)
	for _, tb := range probeTables {
		probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (3, 'Alpha beta', 'beta gamma')`)
	}
	// unicode61: one entry per token occurrence, in term order.
	got := probeDump(t, db, `SELECT term, doc, col, offset, typeof(col) FROM records_fts_v`)
	want := []string{
		`text:"alpha"|int64:3|text:"summary"|int64:0|text:"text"`,
		`text:"beta"|int64:3|text:"summary"|int64:1|text:"text"`,
		`text:"beta"|int64:3|text:"body"|int64:0|text:"text"`,
		`text:"gamma"|int64:3|text:"body"|int64:1|text:"text"`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("records_fts_v =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// trigram: every 3-character window of each column is a term, case folded
	// ("Alpha beta" and "beta gamma" are 10 characters: 8 windows each).
	sub := probeVocab(t, db, "records_fts_sub_v")
	if len(sub) != 16 {
		t.Fatalf("trigram vocab has %d entries, want 16: %+v", len(sub), sub)
	}
	for _, r := range sub {
		if len([]rune(r.term)) != 3 || r.term != strings.ToLower(r.term) || r.doc != 3 {
			t.Fatalf("unexpected trigram entry %+v", r)
		}
	}
	if !slices.ContainsFunc(sub, func(r vocabRow) bool { return r.term == "alp" && r.col == "summary" && r.offset == 0 }) {
		t.Fatalf("no (alp, summary, offset 0) entry in %+v", sub)
	}
}

// TestFTS5VocabOrderIsTermDocColumnOffset: the natural order of the instance
// table is term (as bytes), doc, column INDEX (summary before body, although
// "body" < "summary" by name) and offset, also for multi-byte terms.
func TestFTS5VocabOrderIsTermDocColumnOffset(t *testing.T) {
	db := probeDB(t, probeTables...)
	// ASCII, 2-byte (ß), 3-byte (fullwidth, U+FF41), 3-byte (CJK) and 4-byte
	// (Deseret, U+10428) terms: byte order differs from UTF-16 code unit order
	// for the fullwidth/Deseret pair, which is what a merge must not get wrong.
	terms := []string{"zeta", "alpha", "straße", "ａｂ", "日本", "\U00010428\U00010429", "mid"}
	reversed := slices.Clone(terms)
	slices.Reverse(reversed)
	for _, id := range []int64{9, 2, 5} { // out of rowid order on purpose
		for _, tb := range probeTables {
			// every term occurs in both columns of each document, so for one
			// (term, doc) the entries of "summary" and "body" must be ordered by column index.
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (?, ?, ?)`,
				id, strings.Join(terms, " "), strings.Join(reversed, " "))
		}
	}
	for _, tb := range probeTables {
		got := probeVocab(t, db, tb.vocab)
		want := slices.Clone(got)
		sort.SliceStable(want, func(i, j int) bool {
			a, b := want[i], want[j]
			if c := bytes.Compare([]byte(a.term), []byte(b.term)); c != 0 {
				return c < 0
			}
			if a.doc != b.doc {
				return a.doc < b.doc
			}
			if a.colIndex != b.colIndex {
				return a.colIndex < b.colIndex
			}
			return a.offset < b.offset
		})
		if !slices.Equal(got, want) {
			t.Fatalf("%s: the natural vocab order is not (term bytes, doc, column index, offset)", tb.vocab)
		}
		// the data really exercises the point: a term with entries in both columns of one document
		both := false
		for i := 1; i < len(got); i++ {
			if got[i].term == got[i-1].term && got[i].doc == got[i-1].doc && got[i].col != got[i-1].col {
				both = true
				if got[i-1].col != "summary" || got[i].col != "body" {
					t.Fatalf("%s: for one term and document the summary entry must come first, got %+v then %+v", tb.vocab, got[i-1], got[i])
				}
			}
		}
		if !both {
			t.Fatalf("%s: the data never put one term in both columns of one document", tb.vocab)
		}
		if tb.name == "records_fts" { // the multi-byte terms are present, in byte order
			var seen []string
			for _, r := range got {
				if len(seen) == 0 || seen[len(seen)-1] != r.term {
					seen = append(seen, r.term)
				}
			}
			wantTerms := slices.Clone(terms)
			sort.Strings(wantTerms) // Go compares strings bytewise
			if !slices.Equal(seen, wantTerms) {
				t.Fatalf("distinct terms in vocab order = %q, want %q", seen, wantTerms)
			}
		}
	}
}

// TestFTS5ContentIsIndependentOfBuildHistory: the logical content (vocab stream,
// _docsize, _config and the totals record) is identical whether 3,000 documents
// went in through one transaction or chunks of 50; the segment rows are not.
func TestFTS5ContentIsIndependentOfBuildHistory(t *testing.T) {
	summary, body := probeCorpus(3000)
	one := probeDB(t, probeTables...)
	chunked := probeDB(t, probeTables...)
	for _, tb := range probeTables {
		probeExec(t, one, `BEGIN`)
		probeInsertCorpus(t, one, tb.name, summary, body, 0, 3000)
		probeExec(t, one, `COMMIT`)
		for from := 0; from < 3000; from += 50 {
			probeExec(t, chunked, `BEGIN`)
			probeInsertCorpus(t, chunked, tb.name, summary, body, from, from+50)
			probeExec(t, chunked, `COMMIT`)
		}
		for _, q := range []string{
			`SELECT term, doc, col, offset FROM ` + tb.vocab,
			`SELECT id, sz FROM ` + tb.name + `_docsize ORDER BY id`,
			`SELECT k, v FROM ` + tb.name + `_config ORDER BY k`,
			`SELECT id, block FROM ` + tb.name + `_data WHERE id = 1`,
		} {
			a, b := probeDump(t, one, q), probeDump(t, chunked, q)
			if len(a) == 0 {
				t.Fatalf("%s: empty result", q)
			}
			if !slices.Equal(a, b) {
				t.Errorf("%s differs between a single transaction and chunks of 50 (%d vs %d rows)", q, len(a), len(b))
			}
		}
		seg := `SELECT id, block FROM ` + tb.name + `_data WHERE id <> 1 ORDER BY id`
		if slices.Equal(probeDump(t, one, seg), probeDump(t, chunked, seg)) {
			t.Errorf("%s: the segment rows are identical for both histories; the probe no longer shows why they are not compared", tb.name)
		}
	}
}

// probeLostTerms checks MATCH reachability: for every term the vocab lists
// (a sequential scan of the leaf pages, which never reads _idx), MATCH must
// return exactly the documents the vocab lists. It returns the number of terms
// whose MATCH result differs and the number whose MATCH fails.
func probeLostTerms(t testing.TB, db *sql.DB, tb probeTable) (lost, errs int) {
	t.Helper()
	docs := map[string][]int64{}
	var terms []string
	for _, r := range probeVocab(t, db, tb.vocab) {
		d := docs[r.term]
		if len(d) == 0 {
			terms = append(terms, r.term)
		}
		if len(d) == 0 || d[len(d)-1] != r.doc {
			docs[r.term] = append(d, r.doc)
		}
	}
	for _, term := range terms {
		got, err := queryInts(db, `SELECT rowid FROM `+tb.name+` WHERE `+tb.name+` MATCH ? ORDER BY rowid`, probeQuote(term))
		switch {
		case err != nil:
			errs++
		case !slices.Equal(got, docs[term]):
			lost++
		}
	}
	return lost, errs
}

// probeIntegrity returns the result of PRAGMA integrity_check and the error (nil
// when clean) of FTS5's own integrity-check command.
func probeIntegrity(t testing.TB, db *sql.DB, table string) (pragma string, ftsErr error) {
	t.Helper()
	pragma = strings.Join(probeDump(t, db, `PRAGMA integrity_check`), ";")
	_, ftsErr = db.Exec(`INSERT INTO ` + table + `(` + table + `) VALUES('integrity-check')`) //nolint:gosec // a constant table name in a test
	return pragma, ftsErr
}

const pragmaOK = `text:"ok"`

// TestFTS5IntegrityCheckIgnoresContentTampering: PRAGMA integrity_check cannot
// see a logical change to a contentless index: a deleted _docsize row, and a
// record's postings removed through the 'delete' command, leave it at "ok"
// while a MATCH no longer finds the record. This is why verification compares
// content (P11).
func TestFTS5IntegrityCheckIgnoresContentTampering(t *testing.T) {
	for _, tb := range probeTables {
		t.Run(tb.name, func(t *testing.T) {
			db := probeDB(t, tb)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (1, 'Quarterly report', 'revenue figures')`)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (2, 'Lunch plans', 'sandwich and coffee')`)
			if p, err := probeIntegrity(t, db, tb.name); p != pragmaOK || err != nil {
				t.Fatalf("a healthy index: integrity_check=%s, integrity-check=%v", p, err)
			}
			if got := probeMatch(t, db, tb.name, probeQuote("revenue")); !slices.Equal(got, []int64{1}) {
				t.Fatalf("before tampering MATCH = %v", got)
			}

			// a deleted _docsize row
			probeExec(t, db, `DELETE FROM `+tb.name+`_docsize WHERE id = 2`)
			if p := strings.Join(probeDump(t, db, `PRAGMA integrity_check`), ";"); p != pragmaOK {
				t.Errorf("integrity_check noticed the deleted _docsize row: %s", p)
			}
			if n := probeDump(t, db, `SELECT id FROM `+tb.name+`_docsize`); len(n) != 1 {
				t.Errorf("_docsize has %d rows after the delete, want 1", len(n))
			}

			// postings removed through FTS5's own command: the record disappears from MATCH
			probeExec(t, db, `INSERT INTO `+tb.name+`(`+tb.name+`, rowid, summary, body) VALUES ('delete', 1, 'Quarterly report', 'revenue figures')`)
			if got := probeMatch(t, db, tb.name, probeQuote("revenue")); len(got) != 0 {
				t.Errorf("MATCH still finds the record after its postings were removed: %v", got)
			}
			if got := probeMatch(t, db, tb.name, probeQuote("sandwich")); !slices.Equal(got, []int64{2}) {
				t.Errorf("the untouched record vanished too: %v", got)
			}
			if p := strings.Join(probeDump(t, db, `PRAGMA integrity_check`), ";"); p != pragmaOK {
				t.Errorf("integrity_check noticed the removed postings: %s", p)
			}
		})
	}
}

// TestFTS5IdxTamperLosesHitsThatNeitherIntegrityCheckSees is the R5 probe: can
// tampering with records_fts_idx / records_fts_sub_idx (the b-tree index of
// each segment) make MATCH lose a hit, and which check catches it?
//
// Finding: yes, and NEITHER PRAGMA integrity_check NOR FTS5's own
// 'integrity-check' command catches it when _idx rows are deleted (they only
// validate the rows that remain, not the ones that are missing). Edits that
// leave a wrong row behind (a repointed page) ARE caught as corruption by both.
// A comparison of the vocab does not help either: it is a sequential scan of
// the leaf pages and never reads _idx. So verification of the index must also
// prove MATCH reachability: for every term of the expected vocabulary, the case
// index's MATCH must return exactly the expected documents.
func TestFTS5IdxTamperLosesHitsThatNeitherIntegrityCheckSees(t *testing.T) {
	summary, body := probeCorpus(6000)
	for _, tb := range probeTables {
		t.Run(tb.name, func(t *testing.T) {
			db := probeDB(t, tb)
			probeExec(t, db, `BEGIN`)
			probeInsertCorpus(t, db, tb.name, summary, body, 0, 6000)
			probeExec(t, db, `COMMIT`)
			idx := tb.name + "_idx"
			if n := probeInts(t, db, `SELECT count(*) FROM `+idx+` WHERE term <> x''`); n[0] < 10 {
				t.Fatalf("%s holds only %d non-first rows: the corpus is too small for a multi-page index", idx, n[0])
			}
			if p, err := probeIntegrity(t, db, tb.name); p != pragmaOK || err != nil {
				t.Fatalf("a healthy index: integrity_check=%s, integrity-check=%v", p, err)
			}
			if lost, errs := probeLostTerms(t, db, tb); lost != 0 || errs != 0 {
				t.Fatalf("a healthy index loses %d terms, %d errors", lost, errs)
			}

			silent := map[string]string{
				"delete every non-first row":          `DELETE FROM ` + idx + ` WHERE term <> x''`,
				"delete the last row of each segment": `DELETE FROM ` + idx + ` WHERE (segid, pgno) IN (SELECT segid, max(pgno) FROM ` + idx + ` GROUP BY segid)`,
				"delete every row of one segment":     `DELETE FROM ` + idx + ` WHERE segid = (SELECT max(segid) FROM ` + idx + `)`,
			}
			for name, stmt := range silent {
				probeExec(t, db, `BEGIN`)
				probeExec(t, db, stmt)
				lost, errs := probeLostTerms(t, db, tb)
				p, ftsErr := probeIntegrity(t, db, tb.name)
				probeExec(t, db, `ROLLBACK`)
				t.Logf("%s: %d terms lost, %d MATCH errors, integrity_check=%s, integrity-check=%v", name, lost, errs, p, ftsErr)
				if lost == 0 {
					t.Errorf("%s: MATCH lost no hit (the probe no longer demonstrates the tamper)", name)
				}
				if p != pragmaOK || ftsErr != nil {
					t.Errorf("%s: a check now notices it (integrity_check=%s, integrity-check=%v): the design may rely on it", name, p, ftsErr)
				}
			}

			// other edits are loud: a row left pointing at the wrong place or under the
			// wrong term is caught by PRAGMA integrity_check (design: verify P10); the
			// repointed row is also caught by FTS5's own command
			pick := `(segid, term) = (SELECT segid, term FROM ` + idx + ` WHERE term <> x'' ORDER BY segid, term LIMIT 1 OFFSET 20)`
			for _, e := range []struct {
				name, stmt string
				both       bool
			}{
				{"repoint a row (pgno + 3)", `UPDATE ` + idx + ` SET pgno = pgno + 3 WHERE ` + pick, true},
				{"change the term of a row", `UPDATE ` + idx + ` SET term = x'7a7a7a7a7a' WHERE ` + pick, false},
				{"point a row at page 2", `UPDATE ` + idx + ` SET pgno = 2 WHERE ` + pick, false},
				{"repoint every row (pgno + 1)", `UPDATE ` + idx + ` SET pgno = pgno + 1`, false},
			} {
				probeExec(t, db, `BEGIN`)
				probeExec(t, db, e.stmt)
				p, ftsErr := probeIntegrity(t, db, tb.name)
				probeExec(t, db, `ROLLBACK`)
				t.Logf("%s: integrity_check=%s, integrity-check=%v", e.name, p, ftsErr)
				if p == pragmaOK {
					t.Errorf("%s: PRAGMA integrity_check did not notice it", e.name)
				}
				if e.both && ftsErr == nil {
					t.Errorf("%s: FTS5 integrity-check did not notice it", e.name)
				}
			}
		})
	}
}

// TestFTS5DeleteAndDuplicateInsertAreTheTamperPrimitives: the 'delete' command
// removes the postings of a contentless row given its original text, and
// inserting an existing rowid with other text adds postings: the two operations
// the tamper helpers use.
func TestFTS5DeleteAndDuplicateInsertAreTheTamperPrimitives(t *testing.T) {
	for _, tb := range probeTables {
		t.Run(tb.name, func(t *testing.T) {
			db := probeDB(t, tb)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (1, 'original text', 'body one')`)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (2, 'other record', 'body two')`)
			before := probeVocab(t, db, tb.vocab)

			// a duplicate insert of an existing rowid with other text ADDS postings
			// (the forged text shares no term with the original: see the granularity check below)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (1, 'forged phrase', 'forged words')`)
			if got := probeMatch(t, db, tb.name, probeQuote("forged")); !slices.Equal(got, []int64{1}) {
				t.Fatalf("the forged text is not findable for rowid 1: %v", got)
			}
			if got := probeMatch(t, db, tb.name, probeQuote("original")); !slices.Equal(got, []int64{1}) {
				t.Fatalf("the original postings of rowid 1 vanished: %v", got)
			}
			if after := probeVocab(t, db, tb.vocab); len(after) <= len(before) {
				t.Fatalf("the duplicate insert added no postings (%d -> %d)", len(before), len(after))
			}

			// 'delete' with the text that was inserted removes exactly those postings
			probeExec(t, db, `INSERT INTO `+tb.name+`(`+tb.name+`, rowid, summary, body) VALUES ('delete', 1, 'forged phrase', 'forged words')`)
			if got := probeMatch(t, db, tb.name, probeQuote("forged")); len(got) != 0 {
				t.Fatalf("the 'delete' command left the forged postings: %v", got)
			}
			if after := probeVocab(t, db, tb.vocab); !slices.Equal(after, before) {
				t.Fatalf("after adding and deleting the forged text the vocab differs from the original")
			}

			probeExec(t, db, `INSERT INTO `+tb.name+`(`+tb.name+`, rowid, summary, body) VALUES ('delete', 1, 'original text', 'body one')`)
			if got := probeMatch(t, db, tb.name, probeQuote("original")); len(got) != 0 {
				t.Fatalf("the 'delete' command left the original postings: %v", got)
			}
			if got := probeMatch(t, db, tb.name, probeQuote("record")); !slices.Equal(got, []int64{2}) {
				t.Fatalf("an unrelated row was affected: %v", got)
			}
		})
	}
}

// TestFTS5DeleteRemovesTermRowidPairs: the granularity of the 'delete' command
// is a (term, rowid) pair: a forged text that shares a term with the original
// also removes the original's posting of that term (whatever its offset), so
// tamper helpers must choose text that shares no term with the original.
func TestFTS5DeleteRemovesTermRowidPairs(t *testing.T) {
	db := probeDB(t, probeTables[0])
	probeExec(t, db, `INSERT INTO records_fts(rowid, summary, body) VALUES (1, 'alpha', 'one two')`)
	probeExec(t, db, `INSERT INTO records_fts(rowid, summary, body) VALUES (1, 'beta', 'zero one')`) // adds 'one' at offset 1
	probeExec(t, db, `INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 1, 'beta', 'zero one')`)
	if got := probeMatch(t, db, "records_fts", probeQuote("zero")); len(got) != 0 {
		t.Fatalf("the forged term 'zero' survived its delete: %v", got)
	}
	if got := probeMatch(t, db, "records_fts", probeQuote("one")); len(got) != 0 {
		t.Fatalf("the original posting of the shared term 'one' survived: 'delete' is now finer than (term, rowid): %v", got)
	}
	if got := probeMatch(t, db, "records_fts", probeQuote("two")); !slices.Equal(got, []int64{1}) {
		t.Fatalf("the original posting of 'two' was lost: %v", got)
	}
}

// TestFTS5UnicodeTokenizerFoldsDottedIButNotSharpSOrLigatures documents why Go
// normalization (NormalizeText) runs before the tokenizer. unicode61 with
// remove_diacritics 2 folds case and accents, but NOT: "ß" or "ẞ" to "ss"; a
// ligature ("ﬁ") to "fi"; a fullwidth letter to ASCII; the dotless "ı" to "i".
// It does fold the dotted capital "İ" to "i" (it drops the combining dot), so
// the dotted-I case is covered by the tokenizer and the Go mapping of "ı" (plan
// pipeline step 6) is what is still needed.
func TestFTS5UnicodeTokenizerFoldsDottedIButNotSharpSOrLigatures(t *testing.T) {
	db := probeDB(t, probeTables[0])
	probeExec(t, db, `INSERT INTO records_fts(rowid, summary, body) VALUES
		(1, 'Straße', ''), (2, 'strasse', ''), (3, 'STRAẞE', ''),
		(4, 'İstanbul', ''), (5, 'istanbul', ''), (6, 'ıstanbul', ''),
		(7, 'ﬁsh', ''), (8, 'fish', ''), (9, 'ＡＢＣ', ''), (10, 'abc', ''),
		(11, 'Café', ''), (12, 'cafe'||char(769), ''), (13, 'ſun', ''), (14, 'sun', '')`)
	cases := []struct {
		query string
		want  []int64
	}{
		{"strasse", []int64{2}}, // not the ß rows
		{"straße", []int64{1, 3}},
		{"istanbul", []int64{4, 5}}, // the dotted capital meets i ...
		{"ıstanbul", []int64{6}},    // ... the dotless one does not
		{"fish", []int64{8}},        // no ligature folding
		{"abc", []int64{10}},        // no fullwidth folding
		{"cafe", []int64{11, 12}},   // accents (precomposed and combining) ARE removed
		{"CAFÉ", []int64{11, 12}},   // and case is folded
		{"sun", []int64{13, 14}},    // long s is folded
	}
	for _, c := range cases {
		if got := probeMatch(t, db, "records_fts", probeQuote(c.query)); !slices.Equal(got, c.want) {
			t.Errorf("unicode61 MATCH %q = %v, want %v", c.query, got, c.want)
		}
	}
}

// TestFTS5TrigramFoldsCaseAndDiacritics: the trigram tokenizer as configured
// (case_sensitive 0 remove_diacritics 1) matches substrings regardless of case and accents.
func TestFTS5TrigramFoldsCaseAndDiacritics(t *testing.T) {
	db := probeDB(t, probeTables[1])
	probeExec(t, db, `INSERT INTO records_fts_sub(rowid, summary, body) VALUES (1, 'Crème Brûlée', 'Hello World'), (2, 'plain', 'nothing')`)
	for _, c := range []struct {
		query string
		want  []int64
	}{
		{"creme", []int64{1}},
		{"BRULEE", []int64{1}},
		{"rème br", []int64{1}},
		{"o wor", []int64{1}},
		{"WORLD", []int64{1}},
		{"lai", []int64{2}},
		{"xyz", nil},
	} {
		if got := probeMatch(t, db, "records_fts_sub", probeQuote(c.query)); !slices.Equal(got, c.want) {
			t.Errorf("trigram MATCH %q = %v, want %v", c.query, got, c.want)
		}
	}
}

// spillFiles lists SQLite temporary files that exist now under dir: directory
// entries (Windows lists a delete-on-close file) and, on Linux, open
// descriptors that point there (the unix VFS unlinks a temp file at once, so
// it is visible only through /proc/self/fd, which shows the resolved path, so
// dir is resolved too). Other platforms (macOS) are not supported: the unlinked
// file would be invisible and the positive spill assertion would fail.
func spillFiles(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		out = append(out, e.Name())
	}
	if runtime.GOOS == "linux" {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		fds, _ := os.ReadDir("/proc/self/fd")
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil &&
				strings.HasPrefix(target, dir+string(filepath.Separator)) {
				out = append(out, "fd:"+target)
			}
		}
	}
	return out
}

// useTempDirectory points SQLite's temporary-file directory at dir with
// PRAGMA temp_store_directory and resets it when the test ends (the cleanup
// runs before the directory is removed). The pragma is PROCESS-GLOBAL
// (sqlite3_temp_directory): it outlives the connection it was issued on.
func useTempDirectory(t *testing.T, db *sql.DB, dir string) {
	t.Helper()
	probeExec(t, db, `PRAGMA temp_store_directory = '`+strings.ReplaceAll(dir, "'", "''")+`'`)
	t.Cleanup(resetTempDirectory(t))
}

func resetTempDirectory(t *testing.T) func() {
	return func() {
		reset, err := sql.Open("sqlite", "")
		if err == nil {
			_, err = reset.Exec(`PRAGMA temp_store_directory = ''`)
			_ = reset.Close()
		}
		if err != nil {
			t.Errorf("could not reset temp_store_directory: %v", err)
		}
	}
}

// probeForceSpill writes enough documents into a transaction on db (cache size
// 256 KiB) that SQLite must spill dirty pages to its temporary file; the
// transaction is left open.
func probeForceSpill(t *testing.T, db *sql.DB) {
	t.Helper()
	probeExec(t, db, `PRAGMA cache_size = -256`)
	probeExec(t, db, `CREATE VIRTUAL TABLE spill USING fts5(a, content='')`)
	probeExec(t, db, `BEGIN`)
	for i := 0; i < 20000; i++ {
		probeExec(t, db, `INSERT INTO spill(rowid, a) VALUES (?, ?)`, i, "word"+strconv.Itoa(i)+" other"+strconv.Itoa(i*7)+" third"+strconv.Itoa(i*13))
	}
}

// TestFTS5TemporaryDatabase: the driver opened with an empty path gives a
// private database that supports FTS5 and journal_mode=OFF, spills to a file in
// the directory chosen with temp_store_directory (asserted positively: the
// file was seen) and leaves nothing behind after Close.
func TestFTS5TemporaryDatabase(t *testing.T) {
	dir := t.TempDir()
	decoys := map[string]string{}
	for _, k := range []string{"TMP", "TEMP", "TMPDIR", "SQLITE_TMPDIR"} {
		decoys[k] = t.TempDir() // nothing may land here
		t.Setenv(k, decoys[k])
	}
	db, err := sql.Open("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	useTempDirectory(t, db, dir)

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = OFF`).Scan(&mode); err != nil || mode != "off" {
		t.Fatalf("journal_mode = %q, %v; want off", mode, err)
	}
	for _, tb := range probeTables { // FTS5 with both tokenizers and the vocab table
		probeCreate(t, db, tb)
		probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (1, 'temp db', 'works')`)
		if got := probeMatch(t, db, tb.name, probeQuote("works")); !slices.Equal(got, []int64{1}) {
			t.Fatalf("%s MATCH in the temporary database = %v", tb.name, got)
		}
	}
	if files := spillFiles(t, dir); len(files) != 0 {
		t.Fatalf("a spill file exists before anything was written: %v", files)
	}
	probeForceSpill(t, db)
	files := spillFiles(t, dir)
	if len(files) == 0 {
		t.Fatalf("no spill file appeared in the chosen directory although the cache was exceeded")
	}
	t.Logf("spill files while the transaction is open: %v", files)
	for k, d := range decoys {
		if left := spillFiles(t, d); len(left) != 0 {
			t.Errorf("a spill file reached the decoy directory %s=%s: %v", k, d, left)
		}
	}
	probeExec(t, db, `ROLLBACK`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if left := spillFiles(t, dir); len(left) != 0 {
		t.Fatalf("files left after Close: %v", left)
	}
}

// TestSQLiteTempDirectoryControl is the R12 probe: how to point SQLite's
// temporary-file directory at a directory of our choice. It records which
// mechanisms redirect the spill file of a private database:
//
//   - PRAGMA temp_store_directory and the DSN form _pragma=temp_store_directory(..)
//     work on every platform, but the setting is process-global (the C library's
//     sqlite3_temp_directory), not per connection: it survives the connection and
//     applies to every other database in the process until it is reset
//     (PRAGMA temp_store_directory = ”); the directory must exist (a later spill
//     into a directory that was removed fails with "unable to open database file");
//   - environment variables are honoured per platform only: on Windows TMP (and
//     not TEMP, TMPDIR or SQLITE_TMPDIR); on unix SQLITE_TMPDIR then TMPDIR.
//
// Only the first two facts are asserted: they are what the design relies on.
// The environment results are logged.
func TestSQLiteTempDirectoryControl(t *testing.T) {
	spillsInto := func(t *testing.T, dsn string, setup func(*sql.DB), dir string) bool {
		t.Helper()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if setup != nil {
			setup(db)
		}
		probeForceSpill(t, db)
		found := len(spillFiles(t, dir)) > 0
		probeExec(t, db, `ROLLBACK`)
		_ = db.Close()
		return found
	}

	t.Run("pragma", func(t *testing.T) {
		dir := t.TempDir()
		if !spillsInto(t, "", func(db *sql.DB) { useTempDirectory(t, db, dir) }, dir) {
			t.Fatal("PRAGMA temp_store_directory did not redirect the spill file")
		}
	})
	t.Run("dsn_pragma", func(t *testing.T) {
		dir := t.TempDir()
		t.Cleanup(resetTempDirectory(t)) // the DSN form sets the same process-global value
		if !spillsInto(t, "file:?_pragma=temp_store_directory('"+dir+"')", nil, dir) {
			t.Fatal("the DSN _pragma=temp_store_directory did not redirect the spill file")
		}
	})
	t.Run("pragma_is_process_global", func(t *testing.T) {
		dir := t.TempDir()
		first, err := sql.Open("sqlite", "")
		if err != nil {
			t.Fatal(err)
		}
		useTempDirectory(t, first, dir)
		_ = first.Close() // the connection that issued the pragma is gone
		if !spillsInto(t, "", nil, dir) {
			t.Fatal("a later, unrelated database did not spill into the directory set by an earlier connection: the pragma is no longer process-global")
		}
	})
	t.Run("environment", func(t *testing.T) {
		for _, k := range []string{"TMP", "TEMP", "TMPDIR", "SQLITE_TMPDIR"} {
			dir := t.TempDir()
			t.Setenv(k, dir)
			t.Logf("%-14s redirects the spill file: %v (GOOS=%s)", k, spillsInto(t, "", nil, dir), runtime.GOOS)
		}
	})
}

// TestReadTxContextCancelInterruptsRunningQuery: a runaway recursive query
// through Case.ReadTx stops at its context deadline with
// context.DeadlineExceeded, and the same case answers the next query (the
// connection is not poisoned).
func TestReadTxContextCancelInterruptsRunningQuery(t *testing.T) {
	c, err := Create(t.TempDir(), CreateOptions{ID: "CANCELPROBE", Examiner: "Examiner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = c.ReadTx(ctx, func(h ReadHandle) error {
		var n int64
		return h.QueryRowContext(ctx, `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c`).Scan(&n)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadTx error = %v, want context.DeadlineExceeded", err)
	}

	var v int
	if err := c.ReadTx(context.Background(), func(h ReadHandle) error {
		return h.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v)
	}); err != nil || v != CurrentSchema {
		t.Fatalf("the next query on the same case = %d, %v (the connection is poisoned)", v, err)
	}
}

// TestFTS5DropAndRecreateKeepsSchemaIdentical: DROP TABLE plus the same CREATE
// VIRTUAL TABLE in one transaction, with the vocab table present, leaves
// sqlite_master text identical, also for a table whose _data structure row was
// wrecked: the reindex strategy.
func TestFTS5DropAndRecreateKeepsSchemaIdentical(t *testing.T) {
	for _, tb := range probeTables {
		for _, wreck := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrecked=%v", tb.name, wreck), func(t *testing.T) {
				db := probeDB(t, tb)
				master := func() []string {
					return probeDump(t, db, `SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`)
				}
				probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (1, 'some text', 'more text')`)
				want := master()
				if len(want) < 6 { // the table, the vocab table and the shadow tables
					t.Fatalf("sqlite_master holds only %d objects: %v", len(want), want)
				}
				if wreck {
					probeExec(t, db, `UPDATE `+tb.name+`_data SET block = x'ffffffffffffffffffffffffffff' WHERE id = 10`)
					if _, err := queryInts(db, `SELECT rowid FROM `+tb.name+` WHERE `+tb.name+` MATCH 'text'`); err == nil {
						t.Fatal("wrecking the structure row did not break MATCH: the probe is void")
					}
				}
				probeExec(t, db, `BEGIN`)
				probeExec(t, db, `DROP TABLE `+tb.name)
				probeExec(t, db, tb.ddl)
				probeExec(t, db, `COMMIT`)
				if got := master(); !slices.Equal(got, want) {
					t.Fatalf("sqlite_master changed:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
				// the recreated table is empty, works, and the vocab table reads it
				if got := probeMatch(t, db, tb.name, probeQuote("text")); len(got) != 0 {
					t.Fatalf("the recreated table is not empty: %v", got)
				}
				probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (5, 'fresh text', '')`)
				if got := probeMatch(t, db, tb.name, probeQuote("fresh")); !slices.Equal(got, []int64{5}) {
					t.Fatalf("MATCH after the recreate = %v", got)
				}
				if v := probeVocab(t, db, tb.vocab); len(v) == 0 {
					t.Fatal("the vocab table reads nothing after the recreate")
				}
			})
		}
	}
}

// probeOperatorData is the data of the MATCH operator probes. It holds a term that starts
// with "a" in several shapes (apple, a at the start of a column, a inside a column), zzz, and
// words that look like syntax, so that a probe cannot pass merely because the data lacks a hit.
const probeOperatorData = `(1, 'bbb ccc', 'ddd eee'), (2, 'apple pie', 'zzz'), (3, 'a', 'x'), (4, 'x a b', 'y')`

// TestFTS5MatchStringsAreInert: quoted strings that look like syntax or hold no token return zero
// rows without an error, on data that WOULD match an operator reading of them (see
// TestFTS5MatchOperatorsAreOperators for the inputs that are not inert).
func TestFTS5MatchStringsAreInert(t *testing.T) {
	for _, tb := range probeTables {
		t.Run(tb.name, func(t *testing.T) {
			db := probeDB(t, tb)
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES `+probeOperatorData)
			for _, expr := range []string{
				`""`, `"!!!"`, `"AND"`, `"OR"`, `"NOT"`, `"NEAR"`, `"NEAR(bbb ccc)"`, `"summary:bbb"`,
				`"("`, `"a ""quoted"" b"`, `"bbb" "ccc" "zzz"`,
			} {
				got, err := queryInts(db, `SELECT rowid FROM `+tb.name+` WHERE `+tb.name+` MATCH ? ORDER BY rowid`, expr)
				if err != nil {
					t.Errorf("MATCH %s: %v", expr, err)
					continue
				}
				if len(got) != 0 {
					t.Errorf("MATCH %s found %v, want nothing", expr, got)
				}
			}
		})
	}
}

// TestFTS5MatchOperatorsAreOperators documents, as facts and with the rows each one matches, the
// MATCH inputs that are NOT inert even though they contain a quoted string. A bare "*" after a
// quoted string is a PREFIX query, "^" before one anchors it at the start of a column, a "col :"
// prefix is a column filter, and bare AND, OR, NOT and NEAR( ) are operators. The query compiler
// (Task 7) must therefore neutralise "*", "^", ":" and the keywords itself, never rely on
// quoting alone: it emits a "*" and a column filter only where it means one.
func TestFTS5MatchOperatorsAreOperators(t *testing.T) {
	db := probeDB(t, probeTables[0])
	probeExec(t, db, `INSERT INTO records_fts(rowid, summary, body) VALUES `+probeOperatorData)
	for _, c := range []struct {
		expr string
		want []int64
	}{
		{`"a" *`, []int64{2, 3, 4}},            // prefix: apple, a, a
		{`"a"*`, []int64{2, 3, 4}},             // the same without the blank
		{`"a"`, []int64{3, 4}},                 // without the star only the token a
		{`^"a"`, []int64{3}},                   // anchored at the start of a column: not row 4
		{`summary : "a"`, []int64{3, 4}},       // a column filter
		{`body : "zzz"`, []int64{2}},           // a column filter that excludes the other column
		{`"a" NOT "pie"`, []int64{3, 4}},       // keywords are operators when bare
		{`"a" OR "zzz"`, []int64{2, 3, 4}},     //
		{`"bbb" AND "zzz"`, []int64(nil)},      // AND of two phrases that never meet
		{`"^zzz"`, []int64{2}},                 // inside the quotes "^" is plain punctuation
		{`^"zzz"`, []int64{2}},                 // outside it is the anchor
		{`NEAR("a" "pie", 3)`, []int64(nil)},   // NEAR is a function when bare ("a" is no token of row 2: no hit)
		{`NEAR("apple" "pie", 1)`, []int64{2}}, // ... and it finds adjacent terms
	} {
		got, err := queryInts(db, `SELECT rowid FROM records_fts WHERE records_fts MATCH ? ORDER BY rowid`, c.expr)
		if err != nil {
			t.Errorf("MATCH %s: %v", c.expr, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("MATCH %s = %v, want %v", c.expr, got, c.want)
		}
	}
	// a dangling operator is a syntax error, not an empty result
	if _, err := queryInts(db, `SELECT rowid FROM records_fts WHERE records_fts MATCH ?`, `"zzz" ^`); err == nil {
		t.Error(`MATCH "zzz" ^ did not fail`)
	}

	// the trigram table: "^" inside quotes is a literal character there (it is part of the
	// 4-character phrase), outside it is the anchor
	sub := probeDB(t, probeTables[1])
	probeExec(t, sub, `INSERT INTO records_fts_sub(rowid, summary, body) VALUES `+probeOperatorData)
	for _, c := range []struct {
		expr string
		want []int64
	}{
		{`"^zzz"`, []int64(nil)},
		{`^"zzz"`, []int64{2}},
		{`"zzz"`, []int64{2}},
	} {
		got, err := queryInts(sub, `SELECT rowid FROM records_fts_sub WHERE records_fts_sub MATCH ? ORDER BY rowid`, c.expr)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("trigram MATCH %s = %v, %v, want %v", c.expr, got, err, c.want)
		}
	}
}
