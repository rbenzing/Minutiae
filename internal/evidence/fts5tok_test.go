package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// wantFTS5TablesDigest pins the generated tables (fts5tok_tables.go): a SHA-256 over their canonical
// text. A driver upgrade, or a hand edit of the file, changes it. After regenerating the tables
// (MINUTIAE_REGEN_FTS5_TABLES=1, docs/testing.md), review the diff of fts5tok_tables.go and replace the
// digest with the one the failing test prints.
const wantFTS5TablesDigest = "6ea67dbd9fcfe24b17f6b4b4f12e659f24f79d3a1d8567ffe26371826e164dff"

// ---- the pin -------------------------------------------------------------------------------------

func TestFTS5ReplicaPinMatchesDriver(t *testing.T) {
	db := rawDB(t, ":memory:")
	var version, sourceID string
	if err := db.QueryRow(`SELECT sqlite_version(), sqlite_source_id()`).Scan(&version, &sourceID); err != nil {
		t.Fatal(err)
	}
	if version != fts5ReplicaSQLite {
		t.Errorf("sqlite_version() = %q, fts5ReplicaSQLite = %q: regenerate and review fts5tok_tables.go (docs/testing.md), then update the pin", version, fts5ReplicaSQLite)
	}
	if sourceID != fts5ReplicaSourceID {
		t.Errorf("sqlite_source_id() = %q, fts5ReplicaSourceID = %q: regenerate and review fts5tok_tables.go (docs/testing.md), then update the pin", sourceID, fts5ReplicaSourceID)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "require" {
			f = f[1:]
		}
		if len(f) >= 2 && f[0] == "modernc.org/sqlite" {
			found = append(found, f[0]+" "+f[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("go.mod requires modernc.org/sqlite %d times (%v), want exactly once", len(found), found)
	}
	if found[0] != fts5ReplicaModule {
		t.Errorf("go.mod requires %s but fts5ReplicaModule = %q: regenerate and review fts5tok_tables.go (docs/testing.md), then update the pin", found[0], fts5ReplicaModule)
	}
}

// ---- probing SQLite --------------------------------------------------------------------------------

// tokDB opens a private in-memory database (one connection) with both FTS tables and their vocab tables.
func tokDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA temp_store = MEMORY`); err != nil {
		t.Fatal(err)
	}
	for _, table := range FTSTables() {
		tokCreate(t, db, table)
	}
	return db
}

// tokCreate (re)creates one FTS table and its vocab table, empty.
func tokCreate(t testing.TB, db *sql.DB, table string) {
	t.Helper()
	ddl, _ := FTSTableDDL(table)
	vocab, _ := FTSVocabDDL(table)
	for _, stmt := range []string{`DROP TABLE IF EXISTS ` + ftsVocabName(table), `DROP TABLE IF EXISTS ` + table, ddl, vocab} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// tokDoc is one probe document: the text goes to one column, the other column is empty.
type tokDoc struct {
	text string
	body bool // the text is the body, not the summary
}

// tokIndexed holds what SQLite made of a batch of documents: the postings in (doc, column, position)
// order, grouped by document (docs are numbered from 1), and the _docsize blobs.
type tokIndexed struct {
	postings [][]ftsPosting
	sizes    map[int64][]byte
}

// tokIndex inserts docs (rowid = position + 1) into a fresh table and reads the postings back.
func tokIndex(t testing.TB, db *sql.DB, table string, docs []tokDoc) tokIndexed {
	t.Helper()
	tokCreate(t, db, table)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st, err := tx.Prepare(`INSERT INTO ` + table + ` (rowid, summary, body) VALUES (?, ?, ?)`) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range docs {
		s, b := d.text, ""
		if d.body {
			s, b = "", d.text
		}
		if _, err := st.Exec(i+1, s, b); err != nil {
			t.Fatalf("insert %q into %s: %v", d.text, table, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	out := tokIndexed{postings: make([][]ftsPosting, len(docs)), sizes: map[int64][]byte{}}
	rows, err := db.Query(`SELECT doc, col, offset, term FROM ` + ftsVocabName(table) + ` ORDER BY doc, col, offset`) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p ftsPosting
		if err := rows.Scan(&p.doc, &p.col, &p.off, &p.term); err != nil {
			t.Fatal(err)
		}
		out.postings[p.doc-1] = append(out.postings[p.doc-1], p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	rows, err = db.Query(`SELECT id, sz FROM ` + table + `_docsize`) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var sz []byte
		if err := rows.Scan(&id, &sz); err != nil {
			t.Fatal(err)
		}
		out.sizes[id] = slices.Clone(sz)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	return out
}

// replicaDoc is what the replica says about doc number id (from 1): the postings in the order of
// tokIndexed and the _docsize blob.
func replicaDoc(table string, id int64, d tokDoc) ([]ftsPosting, []byte) {
	tok := ftsTokenizer(table)
	col, n := "summary", uint64(0)
	var ps []ftsPosting
	tok(d.text, func(term []byte, pos int) {
		ps = append(ps, ftsPosting{id, col, int64(pos), string(term)})
		n++
	})
	var sz []byte
	if d.body {
		for i := range ps {
			ps[i].col = "body"
		}
		sz = appendSQLiteVarint(appendSQLiteVarint(nil, 0), n)
	} else {
		sz = appendSQLiteVarint(appendSQLiteVarint(nil, n), 0)
	}
	return ps, sz
}

// diffReplica reports the first differences between SQLite and the replica over a batch of documents.
func diffReplica(table string, docs []tokDoc, got tokIndexed, first int64, report func(string)) {
	for i, d := range docs {
		id := int64(i + 1)
		wantP, wantSz := replicaDoc(table, id, d)
		if !slices.Equal(got.postings[i], wantP) {
			report(fmt.Sprintf("%s: text %+q (document %d): SQLite holds %s, the replica produces %s",
				table, d.text, first+id-1, fmtPostings(got.postings[i]), fmtPostings(wantP)))
			continue
		}
		if sz, ok := got.sizes[id]; !ok || !bytes.Equal(sz, wantSz) {
			report(fmt.Sprintf("%s: text %+q (document %d): _docsize is x'%x', the replica counts x'%x'", table, d.text, first+id-1, got.sizes[id], wantSz))
		}
	}
}

func fmtPostings(ps []ftsPosting) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(' ')
		}
		if i == 6 {
			fmt.Fprintf(&b, "... %d more", len(ps)-i)
			break
		}
		fmt.Fprintf(&b, "(%s,%d,%q)", p.col, p.off, clipText(p.term, 40))
	}
	b.WriteByte(']')
	return b.String()
}

// allScalars returns every scalar value from U+0001 to U+10FFFF.
func allScalars() []rune {
	cps := make([]rune, 0, 0x110000)
	for c := rune(1); c <= utf8.MaxRune; c++ {
		if c >= 0xD800 && c <= 0xDFFF {
			continue
		}
		cps = append(cps, c)
	}
	return cps
}

const tokBatch = 50_000

// ---- the generated tables ----------------------------------------------------------------------------

// derivedTok is what SQLite says about every scalar value.
type derivedTok struct {
	kind []uint8 // by code point
	fold []rune  // by code point; 0 = dropped
}

// deriveFTSTokTables probes every scalar value through a private SQLite, with both DDLs.
//
// The word tokenizer: "a" + c + "b" says whether c separates (two tokens a and b) or belongs to a token
// (one token a + fold(c) + b, where an empty fold means c is dropped). "a " + c + " b" says whether c
// can START a token: a token character that folds to something makes its own token, one that folds to
// nothing and starts a token would still count a position (an empty token), and a mark that can only
// continue a token makes none. The trigram tokenizer has no separators: "a" + c + "bc" gives the two
// trigrams a+f+b and f+b+c, or the one trigram abc when c is dropped. U+FFFE and U+FFFF are probed as
// themselves (SQLite answers as for U+FFFD).
func deriveFTSTokTables(t *testing.T) derivedTok {
	t.Helper()
	cps := allScalars()
	d := derivedTok{kind: make([]uint8, 0x110000), fold: make([]rune, 0x110000)}
	for c := range d.fold {
		d.fold[c] = rune(c)
	}
	u61Fold := make([]rune, 0x110000)
	bad := 0
	fail := func(format string, args ...any) {
		if bad++; bad <= 20 {
			t.Errorf(format, args...)
		}
	}
	db := tokDB(t)
	for lo := 0; lo < len(cps); lo += tokBatch {
		batch := cps[lo:min(lo+tokBatch, len(cps))]
		docs := make([]tokDoc, len(batch))
		for i, c := range batch {
			docs[i] = tokDoc{text: "a" + string(c) + "bc"}
		}
		got := tokIndex(t, db, FTSSubTable, docs)
		for i, c := range batch {
			ps := got.postings[i]
			switch {
			case len(ps) == 1 && ps[0].term == "abc":
				d.fold[c] = 0
			case len(ps) == 2 && strings.HasPrefix(ps[0].term, "a") && strings.HasSuffix(ps[0].term, "b") && utf8.RuneCountInString(ps[0].term) == 3:
				d.fold[c], _ = utf8.DecodeRuneInString(ps[0].term[1:])
			default:
				fail("trigram probe of U+%04X gave %s", c, fmtPostings(ps))
			}
		}
	}
	for lo := 0; lo < len(cps); lo += tokBatch {
		batch := cps[lo:min(lo+tokBatch, len(cps))]
		mid := make([]tokDoc, len(batch))
		start := make([]tokDoc, len(batch))
		for i, c := range batch {
			mid[i] = tokDoc{text: "a" + string(c) + "b"}
			start[i] = tokDoc{text: "a " + string(c) + " b"}
		}
		gm := tokIndex(t, db, FTSWordTable, mid)
		gs := tokIndex(t, db, FTSWordTable, start)
		for i, c := range batch {
			pm, ps := gm.postings[i], gs.postings[i]
			switch {
			case len(pm) == 2 && pm[0].term == "a" && pm[1].term == "b":
				d.kind[c] = ftsKindSep
			case len(pm) == 1 && len(pm[0].term) >= 2 && strings.HasPrefix(pm[0].term, "a") && strings.HasSuffix(pm[0].term, "b"):
				inner := pm[0].term[1 : len(pm[0].term)-1]
				if utf8.RuneCountInString(inner) > 1 {
					fail("word probe of U+%04X gave %s", c, fmtPostings(pm))
				}
				if inner != "" {
					u61Fold[c], _ = utf8.DecodeRuneInString(inner)
				}
				// Can c start a token? Then "a", [fold,] "b" are three tokens, or "b" sits at position 2 (c made an
				// empty token). A mark that can only continue a token leaves "a" and "b" alone.
				if len(ps) < 2 || ps[0].term != "a" || ps[len(ps)-1].term != "b" {
					fail("word start probe of U+%04X gave %s", c, fmtPostings(ps))
				}
				if len(ps) == 3 || (len(ps) == 2 && ps[1].off == 2) {
					d.kind[c] = ftsKindToken
				} else {
					d.kind[c] = ftsKindCont
				}
			default:
				fail("word probe of U+%04X gave %s", c, fmtPostings(pm))
			}
		}
	}
	// Both indexes fold with the same function, so one fold table serves both: where a character belongs
	// to a token of the word index, its fold there must equal its fold in the trigram index.
	for _, c := range cps {
		if d.kind[c] != ftsKindSep && u61Fold[c] != d.fold[c] {
			fail("U+%04X folds to U+%04X in the word index and to U+%04X in the trigram index", c, u61Fold[c], d.fold[c])
		}
	}
	if bad > 0 {
		t.FailNow()
	}
	return d
}

// compressTok turns the per-code-point answers into the ranges of the generated file. Surrogates are
// never probed and take the default (a token character that folds to itself).
func compressTok(d derivedTok) (kinds []ftsKindRange, folds []ftsFoldRange) {
	for c := rune(0); c <= utf8.MaxRune; c++ {
		k := d.kind[c]
		if c == 0 || k == ftsKindToken {
			continue
		}
		if n := len(kinds); n > 0 && kinds[n-1].kind == k && kinds[n-1].hi == c-1 {
			kinds[n-1].hi = c
			continue
		}
		kinds = append(kinds, ftsKindRange{c, c, k})
	}
	var changed []rune
	for c := rune(1); c <= utf8.MaxRune; c++ {
		if d.fold[c] != c {
			changed = append(changed, c)
		}
	}
	for i := 0; i < len(changed); {
		c0 := changed[i]
		f0 := d.fold[c0]
		mode, val := ftsFoldDelta, f0-c0
		switch {
		case f0 == 0:
			mode, val = ftsFoldConst, 0
		case i+1 < len(changed) && changed[i+1] == c0+1 && d.fold[c0+1] == f0:
			mode, val = ftsFoldConst, f0
		}
		j := i
		for j+1 < len(changed) && changed[j+1] == changed[j]+1 {
			c, f := changed[j+1], d.fold[changed[j+1]]
			if (mode == ftsFoldConst && f != val) || (mode == ftsFoldDelta && f-c != val) {
				break
			}
			j++
		}
		folds = append(folds, ftsFoldRange{c0, changed[j], mode, val})
		i = j + 1
	}
	return kinds, folds
}

// tokTablesText is the canonical text of the generated file (before gofmt) and of its digest.
func tokTablesText(kinds []ftsKindRange, folds []ftsFoldRange) string {
	var b strings.Builder
	b.WriteString("// Code generated by TestFTS5TokenTablesAreCurrent (MINUTIAE_REGEN_FTS5_TABLES=1 go test ./internal/evidence -run TestFTS5TokenTablesAreCurrent); DO NOT EDIT.\n\n")
	b.WriteString("package evidence\n\n")
	b.WriteString("// These tables were measured by probing every scalar value through the SQLite of " + fts5ReplicaModule + " (" + fts5ReplicaSQLite + ").\n")
	b.WriteString("// fts5TokKind lists the code points that are not plain token characters of the word index (ftsKindSep:\n")
	b.WriteString("// a separator; ftsKindCont: a mark that continues a token but cannot start one). fts5TokFold lists the code\n")
	b.WriteString("// points whose fold is not themselves (a fold of 0 drops the character); both indexes fold alike.\n\n")
	b.WriteString("var fts5TokKind = []ftsKindRange{\n")
	for _, r := range kinds {
		fmt.Fprintf(&b, "\t{0x%X, 0x%X, %d},\n", r.lo, r.hi, r.kind)
	}
	b.WriteString("}\n\nvar fts5TokFold = []ftsFoldRange{\n")
	for _, r := range folds {
		fmt.Fprintf(&b, "\t{0x%X, 0x%X, %d, %d},\n", r.lo, r.hi, r.mode, r.val)
	}
	b.WriteString("}\n")
	return b.String()
}

func tokTablesDigest(kinds []ftsKindRange, folds []ftsFoldRange) string {
	sum := sha256.Sum256([]byte(tokTablesText(kinds, folds)))
	return hex.EncodeToString(sum[:])
}

func TestFTS5TokenTablesAreCurrent(t *testing.T) {
	d := deriveFTSTokTables(t)
	kinds, folds := compressTok(d)
	digest := tokTablesDigest(kinds, folds)
	if os.Getenv("MINUTIAE_REGEN_FTS5_TABLES") == "1" {
		src, err := format.Source([]byte(tokTablesText(kinds, folds)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("fts5tok_tables.go", src, 0o644); err != nil { //nolint:gosec // a source file of the repository
			t.Fatal(err)
		}
		t.Logf("fts5tok_tables.go written: %d kind ranges, %d fold ranges; wantFTS5TablesDigest = %q", len(kinds), len(folds), digest)
		return
	}
	// the file against SQLite, code point by code point
	tab := ftsTokTables()
	bad := 0
	for c := rune(1); c <= utf8.MaxRune; c++ {
		if c >= 0xD800 && c <= 0xDFFF {
			continue
		}
		k, f := tab.get(c)
		if d.kind[c] != k || d.fold[c] != f {
			if bad++; bad <= 10 {
				t.Errorf("U+%04X: SQLite says kind %d fold U+%04X, fts5tok_tables.go says kind %d fold U+%04X", c, d.kind[c], d.fold[c], k, f)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d code points differ: regenerate with MINUTIAE_REGEN_FTS5_TABLES=1 and review the diff (docs/testing.md)", bad)
	}
	// the file is exactly what the generator writes, and it is the pinned one
	if !slices.Equal(kinds, fts5TokKind) || !slices.Equal(folds, fts5TokFold) {
		t.Errorf("fts5tok_tables.go is not the generator's output (%d/%d ranges in the file, %d/%d generated): regenerate", len(fts5TokKind), len(fts5TokFold), len(kinds), len(folds))
	}
	if got := tokTablesDigest(fts5TokKind, fts5TokFold); got != wantFTS5TablesDigest {
		t.Errorf("the tables hash to %q, wantFTS5TablesDigest = %q: review fts5tok_tables.go, then update the digest", got, wantFTS5TablesDigest)
	}
	if digest != wantFTS5TablesDigest {
		t.Errorf("SQLite's answers hash to %q, wantFTS5TablesDigest = %q", digest, wantFTS5TablesDigest)
	}
}

// ---- the differential ---------------------------------------------------------------------------------

// The four contexts of a code point c. Two letters on each side keep the trigram windows meaningful.
var tokContexts = []struct {
	name  string
	frame func(c string) string
}{
	{"alone", func(c string) string { return c }},
	{"start", func(c string) string { return c + "yz" }},
	{"end", func(c string) string { return "xw" + c }},
	{"middle", func(c string) string { return "xw" + c + "yz" }},
}

// tokPack is the number of probes of one differential document. The probes of a document are framed by
// ASCII letters and separated by a space, so what one code point does cannot reach the next probe; a
// difference in a pack is then located by indexing its probes one by one.
const tokPack = 8

func TestFTS5ReplicaMatchesSQLiteOnEveryCodePoint(t *testing.T) {
	cps := allScalars()
	for _, table := range FTSTables() {
		for _, ctx := range tokContexts {
			t.Run(table+"/"+ctx.name, func(t *testing.T) {
				t.Parallel()
				db := tokDB(t)
				bad := 0
				report := func(s string) {
					if bad++; bad <= 10 {
						t.Error(s)
					}
				}
				for lo := 0; lo < len(cps); lo += tokBatch * tokPack {
					batch := cps[lo:min(lo+tokBatch*tokPack, len(cps))]
					var docs []tokDoc
					var packs [][]rune
					for i := 0; i < len(batch); i += tokPack {
						pack := batch[i:min(i+tokPack, len(batch))]
						frames := make([]string, len(pack))
						for j, c := range pack {
							frames[j] = ctx.frame(string(c))
						}
						docs = append(docs, tokDoc{text: strings.Join(frames, " "), body: (i/tokPack)%2 == 1})
						packs = append(packs, pack)
					}
					got := tokIndex(t, db, table, docs)
					// name the code points: index each probe of every failing pack alone
					for i, d := range docs {
						if ps, sz := replicaDoc(table, int64(i+1), d); slices.Equal(ps, got.postings[i]) && bytes.Equal(sz, got.sizes[int64(i+1)]) {
							continue
						}
						if bad >= 10 { // enough to see what is wrong; the rest would only re-index the packs one by one
							break
						}
						before := bad
						for _, c := range packs[i] {
							one := []tokDoc{{text: ctx.frame(string(c))}}
							diffReplica(table, one, tokIndex(t, db, table, one), 1, func(s string) { report(fmt.Sprintf("U+%04X: %s", c, s)) })
						}
						if bad == before { // a probe leaked into its neighbour: never let that pass
							report(fmt.Sprintf("the pack %+q differs from the replica, though no probe differs alone", clipText(d.text, 120)))
						}
					}
				}
				if bad > 0 {
					t.Fatalf("%d code points differ", bad)
				}
			})
		}
	}
}

func TestFTS5ReplicaTruncatesLongTokens(t *testing.T) {
	// 3-byte characters: 32768 = 3*10922 + 2, so the cut lands inside a character
	han := strings.Repeat("\u4e16", 11_000)
	docs := []tokDoc{
		{text: strings.Repeat("a", 40_000)},
		{text: strings.Repeat("a", 32_768)},
		{text: strings.Repeat("a", 32_769)},
		{text: han, body: true},
		{text: "x " + strings.Repeat("b", 33_000) + " y " + han + " z"},
		{text: strings.Repeat("a", 32_768) + " tail"},
	}
	for _, table := range FTSTables() {
		db := tokDB(t)
		got := tokIndex(t, db, table, docs)
		bad := 0
		diffReplica(table, docs, got, 1, func(s string) {
			bad++
			t.Error(clipText(s, 300))
		})
		if bad > 0 {
			continue
		}
		if table == FTSWordTable {
			// the cut itself: SQLite's term for the 40,000-byte token is 32,768 bytes long
			if n := len(got.postings[0][0].term); n != fts5MaxTokenSize {
				t.Errorf("SQLite cut the token at %d bytes, want %d", n, fts5MaxTokenSize)
			}
			if n := len(got.postings[3][0].term); n != fts5MaxTokenSize {
				t.Errorf("SQLite cut the 3-byte-character token at %d bytes, want %d", n, fts5MaxTokenSize)
			}
		}
	}
}

func TestFTS5ReplicaTokenSemantics(t *testing.T) {
	// small, named cases of the rules in the file comment, replica only (the differential proves SQLite agrees)
	collect := func(table, text string) string {
		var parts []string
		ftsTokenizer(table)(text, func(term []byte, pos int) { parts = append(parts, fmt.Sprintf("%d:%s", pos, term)) })
		return strings.Join(parts, " ")
	}
	for _, c := range []struct{ table, in, want string }{
		{FTSWordTable, "", ""},
		{FTSWordTable, "Hello, World", "0:hello 1:world"},
		{FTSWordTable, "caf\u00e9", "0:cafe"},
		{FTSWordTable, "a\u0300b", "0:ab"},             // a removed mark inside a token
		{FTSWordTable, " \u0300b \u0300 c", "0:b 1:c"}, // a removed mark never starts a token
		{FTSWordTable, "a\u0305b", "0:a 1:b"},          // a mark that is not removed separates
		{FTSSubTable, "ab", ""},
		{FTSSubTable, "abcd", "0:abc 1:bcd"},
		{FTSSubTable, "a\u0300bc", "0:abc"}, // a dropped mark is not a character
		{FTSSubTable, "AB c", "0:ab  1:b c"},
	} {
		if got := collect(c.table, c.in); got != c.want {
			t.Errorf("%s %q = %q, want %q", c.table, c.in, got, c.want)
		}
	}
}

func TestSQLiteVarintEncoderRoundTrips(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<56 - 1, 1 << 56, 1<<63 + 5, 1<<64 - 1} {
		b := appendSQLiteVarint(nil, v)
		got, n := SQLiteVarint(b)
		if got != v || n != len(b) {
			t.Errorf("varint %d: encoded x'%x', decoded %d (%d bytes)", v, b, got, n)
		}
	}
}

// ---- the probe corpus and the self-test -----------------------------------------------------------------

func TestFTSProbeCorpusCoversEveryTableRange(t *testing.T) {
	seen := map[rune]bool{}
	long := false
	for _, s := range ftsProbeCorpus {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			t.Fatalf("probe %q is not valid UTF-8 without NUL", clipText(s, 40))
		}
		for _, c := range s {
			seen[c] = true
		}
		if tokenLen(s) > fts5MaxTokenSize {
			long = true
		}
	}
	covered := func(lo, hi rune) bool {
		for c := lo; c <= hi; c++ {
			if seen[c] {
				return true
			}
			if c-lo > 1<<16 {
				break
			}
		}
		return false
	}
	if len(fts5TokKind) == 0 || len(fts5TokFold) == 0 {
		t.Fatal("fts5tok_tables.go holds no ranges")
	}
	for _, r := range fts5TokKind {
		if !covered(r.lo, r.hi) {
			t.Errorf("kind range U+%04X..U+%04X has no code point in ftsProbeCorpus", r.lo, r.hi)
		}
	}
	for _, r := range fts5TokFold {
		if !covered(r.lo, r.hi) {
			t.Errorf("fold range U+%04X..U+%04X has no code point in ftsProbeCorpus", r.lo, r.hi)
		}
	}
	if !long {
		t.Errorf("ftsProbeCorpus holds no token longer than %d bytes", fts5MaxTokenSize)
	}
}

// tokenLen is the length of the longest run of non-space bytes of s.
func tokenLen(s string) int {
	best := 0
	for f := range strings.FieldsSeq(s) {
		best = max(best, len(f))
	}
	return best
}

func TestFTSSelfTestPassesOnThisSQLite(t *testing.T) {
	ref, err := ftsSelfTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range FTSTables() {
		var found bool
		for _, row := range ref.config[table] {
			if row.key.repr == "version" {
				found = true
			}
		}
		if !found {
			t.Errorf("config of %s holds no version row: %v", table, ref.config[table])
		}
	}
}

func TestFTSSelfTestFailsClosedOnAReplicaMismatch(t *testing.T) {
	type tokenizer = func(string, func([]byte, int))
	// every way a wrong replica can differ from SQLite: a missing token (the sizes differ), a changed term, a
	// shifted position, and a repeated token (the sizes differ again)
	variants := []struct {
		name string
		wrap func(inner tokenizer) tokenizer
		want string // a part of the error text: which comparison caught it
	}{
		{"drops the last token", func(inner tokenizer) tokenizer {
			return func(text string, emit func([]byte, int)) {
				var toks [][]byte
				inner(text, func(term []byte, _ int) { toks = append(toks, bytes.Clone(term)) })
				for i, tk := range toks[:max(0, len(toks)-1)] {
					emit(tk, i)
				}
			}
		}, "_docsize"},
		{"changes a term", func(inner tokenizer) tokenizer {
			return func(text string, emit func([]byte, int)) {
				inner(text, func(term []byte, pos int) { emit(append(bytes.Clone(term), 'x'), pos) })
			}
		}, "where the replica produces"},
		{"shifts the positions", func(inner tokenizer) tokenizer {
			return func(text string, emit func([]byte, int)) {
				inner(text, func(term []byte, pos int) { emit(term, pos+1) })
			}
		}, "where the replica produces"},
		{"repeats the first token", func(inner tokenizer) tokenizer {
			return func(text string, emit func([]byte, int)) {
				n := 0
				inner(text, func(term []byte, pos int) {
					if n == 0 {
						emit(term, pos)
						n++
					}
					emit(term, pos+n)
				})
			}
		}, "_docsize"},
	}
	for _, table := range FTSTables() {
		for _, v := range variants {
			t.Run(table+"/"+v.name, func(t *testing.T) {
				old := ftsReplicaOverride
				t.Cleanup(func() { ftsReplicaOverride = old })
				ftsReplicaOverride = func(name string) tokenizer {
					switch {
					case name != table:
						return nil
					case name == FTSWordTable:
						return v.wrap(tokenizeUnicode61)
					}
					return v.wrap(tokenizeTrigram)
				}
				_, err := ftsSelfTest(context.Background())
				if !errors.Is(err, errFTSReplica) {
					t.Fatalf("a replica that %s gave %v, want errFTSReplica", v.name, err)
				}
				if !strings.Contains(err.Error(), v.want) {
					t.Errorf("the error %q does not come from the %q comparison", err, v.want)
				}
			})
		}
	}
}

func TestFTSSelfTestErrorIsNotAReplicaMismatchWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ftsSelfTest(ctx)
	if err == nil {
		t.Fatal("a cancelled context passed the self-test")
	}
	if errors.Is(err, errFTSReplica) {
		t.Fatalf("a cancelled context is not a replica mismatch: %v", err)
	}
}

func TestFTS5PinErrorNamesAnyOtherSQLite(t *testing.T) {
	if err := fts5PinError(fts5ReplicaSQLite, fts5ReplicaSourceID); err != nil {
		t.Fatalf("the pinned SQLite is refused: %v", err)
	}
	for _, c := range [][2]string{
		{"3.53.5", fts5ReplicaSourceID},
		{fts5ReplicaSQLite, "2026-07-24 19:02:57 other"},
		{"", ""},
	} {
		if err := fts5PinError(c[0], c[1]); !errors.Is(err, errFTSReplica) {
			t.Errorf("fts5PinError(%q, %q) = %v, want errFTSReplica", c[0], c[1], err)
		}
	}
}
