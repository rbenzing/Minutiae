package evidence_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// corpusBatch bounds the documents of one SQLite index, so the largest corpus never holds all its postings.
const corpusBatch = 400

// corpusCompare indexes docs (rowid = position + 1) in both FTS tables of a private SQLite and checks that
// the Go replica of each tokenizer gives the same (term, column, position) list for every document and the
// same token count per column as the _docsize row.
func corpusCompare(t *testing.T, docs []evidence.FTSDoc) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, table := range evidence.FTSTables() {
		ddl, _ := evidence.FTSTableDDL(table)
		vocab, _ := evidence.FTSVocabDDL(table)
		bad := 0
		for lo := 0; lo < len(docs); lo += corpusBatch {
			batch := docs[lo:min(lo+corpusBatch, len(docs))]
			for _, stmt := range []string{`DROP TABLE IF EXISTS ` + table + `_v`, `DROP TABLE IF EXISTS ` + table, ddl, vocab} {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			for i, d := range batch {
				if _, err := db.Exec(`INSERT INTO `+table+` (rowid, summary, body) VALUES (?, ?, ?)`, i+1, d.Summary, d.Body); err != nil { //nolint:gosec // the table is one of the two FTS table constants
					t.Fatalf("index document %d in %s: %v", lo+i, table, err)
				}
			}
			type posting struct {
				doc       int64
				col, term string
				off       int64
			}
			var want []posting
			sizes := make([][2]uint64, len(batch))
			for i, d := range batch {
				for _, c := range []struct {
					name, text string
					idx        int
				}{{"body", d.Body, 1}, {"summary", d.Summary, 0}} { // ORDER BY col: "body" < "summary"
					toks := evidence.FTSReplicaTokens(table, c.text)
					for _, tk := range toks {
						want = append(want, posting{int64(i + 1), c.name, tk.Term, int64(tk.Pos)})
					}
					sizes[i][c.idx] = uint64(len(toks))
				}
			}
			rows, err := db.Query(`SELECT doc, col, offset, term FROM ` + table + `_v ORDER BY doc, col, offset`) //nolint:gosec // the table is one of the two FTS table constants
			if err != nil {
				t.Fatal(err)
			}
			k := 0
			for rows.Next() {
				var g posting
				if err := rows.Scan(&g.doc, &g.col, &g.off, &g.term); err != nil {
					t.Fatal(err)
				}
				if k >= len(want) || g != want[k] {
					if bad++; bad <= 5 {
						exp := "nothing"
						if k < len(want) {
							exp = fmt.Sprintf("%+v", clipPosting(want[k].doc, want[k].col, want[k].off, want[k].term))
						}
						t.Errorf("%s: document %d: SQLite holds %+v where the replica has %s", table, int64(lo)+g.doc-1, clipPosting(g.doc, g.col, g.off, g.term), exp)
					}
					break
				}
				k++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			if k < len(want) && bad == 0 {
				bad++
				t.Errorf("%s: batch at %d: SQLite holds %d postings, the replica produces %d", table, lo, k, len(want))
			}
			if bad > 0 {
				continue
			}
			sz, err := db.Query(`SELECT id, sz FROM ` + table + `_docsize ORDER BY id`) //nolint:gosec // the table is one of the two FTS table constants
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for sz.Next() {
				var id int64
				var blob []byte
				if err := sz.Scan(&id, &blob); err != nil {
					t.Fatal(err)
				}
				var a, b uint64
				v1, n1 := evidence.SQLiteVarint(blob)
				v2, n2 := evidence.SQLiteVarint(blob[n1:])
				a, b = v1, v2
				if id != int64(n+1) || n1 == 0 || n2 == 0 || n1+n2 != len(blob) || a != sizes[n][0] || b != sizes[n][1] {
					if bad++; bad <= 5 {
						t.Errorf("%s: document %d: _docsize is x'%x', the replica counts %d and %d tokens", table, lo+n, blob, sizes[n][0], sizes[n][1])
					}
				}
				n++
			}
			_ = sz.Close()
			if n != len(batch) && bad == 0 {
				bad++
				t.Errorf("%s: batch at %d: _docsize holds %d rows for %d documents", table, lo, n, len(batch))
			}
		}
	}
}

func clipPosting(doc int64, col string, off int64, term string) string {
	if len(term) > 40 {
		term = term[:40] + "..."
	}
	return fmt.Sprintf("(doc %d, %s, pos %d, %q)", doc, col, off, term)
}

// corpusDocs normalizes records the way the index does: every document goes through NewFTSDoc first.
func corpusDocs(texts [][2]string) []evidence.FTSDoc {
	var docs []evidence.FTSDoc
	for _, s := range texts {
		body := s[1]
		if d, ok := evidence.NewFTSDoc(int64(len(docs)+1), s[0], &body); ok {
			docs = append(docs, d)
		}
	}
	return docs
}

func recordTexts(recs []records.Record) [][2]string {
	out := make([][2]string, len(recs))
	for i, r := range recs {
		out[i] = [2]string{r.Summary, r.Body}
	}
	return out
}

// TestFTS5ReplicaMatchesSQLiteOnCorpora is Review Focus 1: the replicas equal SQLite on the corpora the
// rest of the suite indexes and on the tokenizer edges.
func TestFTS5ReplicaMatchesSQLiteOnCorpora(t *testing.T) {
	corpora := map[string]func() [][2]string{
		"TextRecords": func() [][2]string {
			recs, _ := recordstest.TextRecords("art")
			return recordTexts(recs)
		},
		"ftsText": func() [][2]string {
			var out [][2]string
			for i := 1; i <= 300; i++ {
				s := fmt.Sprintf("alpha%d beta%d gamma%d", i, i*7, i*13)
				b := fmt.Sprintf("body of record %d with delta%d and epsilon%d lorem ipsum dolor sit amet", i, i*3, i*11)
				out = append(out, [2]string{s, b})
			}
			return out
		},
		"bulkRecords": func() [][2]string {
			var out [][2]string
			for i := range 3000 {
				s := fmt.Sprintf("alpha%d beta%d gamma%d", i, i*7, i*13)
				b := fmt.Sprintf("body of record %d with delta%d and epsilon%d lorem ipsum", i, i*3, i*11)
				out = append(out, [2]string{s, b})
			}
			return out
		},
		"Scale": func() [][2]string {
			sc := recordstest.NewScale(recordstest.ScaleConfig{Seed: 7, Artifacts: []string{"a1", "a2", "a3", "a4"}})
			return recordTexts(sc.Block(0, 10_000))
		},
		"normalize inputs": func() [][2]string {
			var out [][2]string
			ins := evidence.NormalizeCorpusInputs()
			for i, s := range ins {
				out = append(out, [2]string{s, ins[(i+1)%len(ins)]})
			}
			return out
		},
		"edge strings": func() [][2]string {
			return [][2]string{
				{strings.Repeat("a", 40_000), strings.Repeat("\u4e16", 11_000)},
				{"x\ufffey\uffffz", "\ufffd\ufffe\uffff"},
				{"e\u0301 a\u0300 o\u0323\u0302 \u0345 \u0305", "caf\u00e9 \u00e0\u00e8\u00ec \u00f1 \u1ea1\u0301"},
				{"\u0e17\u0e14\u0e2a\u0e2d\u0e1a \u0e44\u0e17\u0e22 \u0e01\u0e32\u0e23", "\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8 \ud55c\uad6d\uc5b4"},
				{"abc123def 12ab 34", "a1b2c3 99x ABC"},
				{"\U0001f600 smile \U0001f1fa\U0001f1f8 \U00010400\U00010428", "\U0001d7ce\U0001d7cf digits"},
			}
		},
	}
	for name, mk := range corpora {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			texts := mk()
			corpusCompare(t, corpusDocs(texts))
		})
	}
	t.Run("probe corpus", func(t *testing.T) {
		t.Parallel()
		probe := evidence.FTSProbeCorpus()
		var raw []evidence.FTSDoc
		var norm [][2]string
		for i, s := range probe {
			next := probe[(i+1)%len(probe)]
			raw = append(raw, evidence.FTSDoc{ID: int64(i + 1), Summary: s, Body: next})
			norm = append(norm, [2]string{s, next})
		}
		corpusCompare(t, raw)
		corpusCompare(t, corpusDocs(norm))
	})
}
