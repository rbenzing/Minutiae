package records_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Searches over the corpus: (table, MATCH expression) and the ids they must find. The expressions
// are normalized text (the index holds normalized text), so they are what the query compiler will
// emit later.
var corpusSearches = []struct {
	table, match string
	ids          []int64
}{
	{evidence.FTSWordTable, `"cafe"`, []int64{1, 2}},
	{evidence.FTSWordTable, `"strasse"`, []int64{3}},
	{evidence.FTSWordTable, `"abc123"`, []int64{4}},
	{evidence.FTSWordTable, `"bodyneedle"`, []int64{14}},
	{evidence.FTSWordTable, `"summaryneedle"`, []int64{15}},
	{evidence.FTSWordTable, `"needleattheend"`, []int64{16}},
	{evidence.FTSSubTable, `"afe au l"`, []int64{1, 2}},
	{evidence.FTSSubTable, `"dyneedl"`, []int64{14}},
	{evidence.FTSSubTable, `"needleat"`, []int64{16}},
}

func corpusIndexedCase(t *testing.T) (*evidence.Case, evidence.ManifestRecord) {
	t.Helper()
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, art
}

func checkCorpusSearches(t *testing.T, c *evidence.Case, when string) {
	t.Helper()
	for _, s := range corpusSearches {
		if got := recordstest.FTSMatch(t, c, s.table, s.match); !slices.Equal(got, s.ids) {
			t.Errorf("%s: %s MATCH %s = %v, want %v", when, s.table, s.match, got, s.ids)
		}
	}
}

// indexContent is what the index holds, independent of how it was built: the vocab streams, the
// docsize and config rows and the totals record of both tables (the segment rows differ with the
// build history, which is why they are not part of it).
func indexContent(t *testing.T, c *evidence.Case) string {
	t.Helper()
	var parts []string
	for _, table := range evidence.FTSTables() {
		for _, q := range []string{
			`SELECT term || '/' || doc || '/' || col || '/' || offset FROM ` + table + `_v ORDER BY term, doc, col, offset`,
			`SELECT id || ':' || hex(sz) FROM ` + table + `_docsize ORDER BY id`,
			`SELECT k || '=' || v FROM ` + table + `_config ORDER BY k`,
			`SELECT hex(block) FROM ` + table + `_data WHERE id = 1`,
		} {
			parts = append(parts, strings.Join(column(t, c, q), "\n"))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n--\n")))
	return hex.EncodeToString(sum[:])
}

func column(t *testing.T, c *evidence.Case, q string) []string {
	t.Helper()
	var out []string
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.Query(q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// tableDump is a canonical dump of a table: every row rendered with the types of its values, sorted.
func tableDump(t *testing.T, c *evidence.Case, table string) string {
	t.Helper()
	var lines []string
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.Query(`SELECT * FROM ` + table)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			var sb strings.Builder
			for _, v := range vals {
				fmt.Fprintf(&sb, "%T:%v|", v, v)
			}
			lines = append(lines, sb.String())
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func schemaRows(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	return column(t, c, `SELECT type || '|' || name || '|' || tbl_name || '|' || coalesce(sql, '') FROM sqlite_master ORDER BY type, name`)
}

// TestReindexRebuildsIndexFromRecords: an index emptied behind the back of the writer finds nothing;
// the reindex rebuilds it from the records alone and every search hits again, with the index holding
// exactly what the writer's own incremental build held.
func TestReindexRebuildsIndexFromRecords(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	checkCorpusSearches(t, c, "after the ingest")
	content := indexContent(t, c)

	recordstest.EmptyFTSIndex(t, c.Dir)
	for _, s := range corpusSearches {
		if got := recordstest.FTSMatch(t, c, s.table, s.match); len(got) != 0 {
			t.Fatalf("the emptied index still finds %v for %s", got, s.match)
		}
	}
	res, err := c.ReindexText(ctx, evidence.ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != corpusRecords || res.Indexed != corpusIndexed || res.NormVersion != evidence.FTSNormVersion() || res.ReindexID == "" {
		t.Errorf("result = %+v", res)
	}
	for _, table := range evidence.FTSTables() {
		if res.Docs[table] != corpusIndexed {
			t.Errorf("Docs[%s] = %d, want %d", table, res.Docs[table], corpusIndexed)
		}
	}
	checkCorpusSearches(t, c, "after the reindex")
	if got := indexContent(t, c); got != content {
		t.Error("the rebuilt index does not hold what the writer's incremental build held")
	}
	if err := c.RequireIndexCurrent(ctx); err != nil {
		t.Error(err)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Errorf("verify = %+v, %v", rep, err)
	}
}

// TestReindexAuditDetails: the start and the done entry decode with DecodeDetails and carry the
// versions, the counts, and the per-table document counts read from the _docsize tables.
func TestReindexAuditDetails(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	recordstest.SetFTSNormVersion(t, c.Dir, "fts0/old")
	before := len(auditOf(t, c, ""))
	res, err := c.ReindexText(ctx, evidence.ReindexOptions{ChunkRows: 5})
	if err != nil {
		t.Fatal(err)
	}
	tail := auditOf(t, c, "")[before:]
	if len(tail) != 2 || tail[0].Action != evidence.ActionReindex || tail[1].Action != evidence.ActionReindexDone || tail[1].Seq != tail[0].Seq+1 {
		t.Fatalf("audit after the reindex = %+v, want records.reindex then records.reindex.done", tail)
	}
	start, done := details[evidence.ReindexStart](t, tail[0]), details[evidence.ReindexDone](t, tail[1])
	if start.ReindexID != res.ReindexID || done.ReindexID != res.ReindexID {
		t.Errorf("ids: start %q, done %q, result %q", start.ReindexID, done.ReindexID, res.ReindexID)
	}
	if start.FromNormVersion != "fts0/old" || start.NormVersion != evidence.FTSNormVersion() || start.Records != corpusRecords || !slices.Equal(start.Tables, evidence.FTSTables()) {
		t.Errorf("start = %+v", start)
	}
	if done.NormVersion != evidence.FTSNormVersion() || done.RecordsIndexed != corpusIndexed {
		t.Errorf("done = %+v", done)
	}
	for _, table := range evidence.FTSTables() {
		if want := int64(docsizeCount(t, c, table)); done.Docs[table] != want || want != corpusIndexed {
			t.Errorf("done.Docs[%s] = %d, _docsize holds %d, want %d", table, done.Docs[table], want, corpusIndexed)
		}
	}
	if len(done.Docs) != 2 {
		t.Errorf("done.Docs = %v, want exactly the two tables", done.Docs)
	}
	if res.FromNormVersion != "fts0/old" || res.Records != corpusRecords || res.Indexed != corpusIndexed {
		t.Errorf("result = %+v", res)
	}
}

// TestReindexNeverTouchesRecords: every record table is byte for byte the same after a reindex, the
// audit log grew by exactly records.reindex and records.reindex.done, and verify still passes.
func TestReindexNeverTouchesRecords(t *testing.T) {
	c, art := corpusIndexedCase(t)
	// a second run, so superseded/run tables are not empty either
	recs, _ := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, records.Parser{Name: testParser.Name, Version: "2.0", Hash: "def456"}, []string{art.ID}, recs[:5])
	tables := []string{"records", "record_times", "record_batches", "parsers", "record_runs", "record_run_artifacts", "record_superseded", "artifacts"}
	snapshot := func() string {
		var parts []string
		for _, tbl := range tables {
			sum := sha256.Sum256([]byte(tableDump(t, c, tbl)))
			parts = append(parts, tbl+"="+hex.EncodeToString(sum[:]))
		}
		parts = append(parts, "next_id="+scalar[string](t, c, `SELECT value FROM records_meta WHERE key = 'next_id'`))
		return strings.Join(parts, "\n")
	}
	if tableDump(t, c, "records") == "" || tableDump(t, c, "record_superseded") == "" {
		t.Fatal("the scenario needs rows in the record tables and in record_superseded")
	}
	before, auditBefore := snapshot(), len(auditOf(t, c, ""))
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{ChunkRows: 3}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Errorf("a record table changed:\n--- before\n%s\n--- after\n%s", before, after)
	}
	tail := auditOf(t, c, "")[auditBefore:]
	var actions []string
	for _, e := range tail {
		actions = append(actions, e.Action)
	}
	if !slices.Equal(actions, []string{evidence.ActionReindex, evidence.ActionReindexDone}) {
		t.Errorf("the audit log grew by %v, want exactly records.reindex and records.reindex.done", actions)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() || len(rep.Notices) != 0 || rep.RecordsChecked != corpusRecords+5 {
		t.Errorf("verify = %+v, %v", rep, err)
	}
}

// TestReindexUpgradedV2Case: a v2 case with records is upgraded (index unbuilt), reindexed (every
// record that has text is in both indexes, superseded runs included) and then verifies clean with no
// notice and accepts a new ingest.
func TestReindexUpgradedV2Case(t *testing.T) {
	dir := recordstest.CopyV2Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if up, err := c.Upgrade(); err != nil || up.RecordsToIndex != 12 {
		t.Fatalf("Upgrade = %+v, %v", up, err)
	}
	if err := c.RequireIndexCurrent(ctx); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Fatalf("RequireIndexCurrent after the upgrade = %v", err)
	}
	res, err := c.ReindexText(ctx, evidence.ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 12 || res.FromNormVersion != "" || res.Indexed < 10 || res.Indexed > 12 {
		t.Fatalf("result = %+v", res)
	}
	// the ids of the records that have text, computed from the records themselves
	var want []int64
	for _, id := range column(t, c, `SELECT id FROM records WHERE summary <> '' OR coalesce(body, '') <> '' ORDER BY id`) {
		var n int64
		_, _ = fmt.Sscan(id, &n)
		want = append(want, n)
	}
	if int64(len(want)) != res.Indexed {
		t.Fatalf("the records with text are %v but %d were indexed", want, res.Indexed)
	}
	for _, table := range evidence.FTSTables() {
		if got := recordstest.IndexedIDs(t, c, table); !slices.Equal(got, want) {
			t.Errorf("%s holds ids %v, want %v", table, got, want)
		}
	}
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, `"cafe"`); len(got) != 2 {
		t.Errorf("cafe is found in %v, want one record per run (both runs are indexed)", got)
	}
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, err := r.Count(ctx, records.Filter{}, 100); err != nil || n != 6 {
		t.Errorf("default count = %d, %v; want the 6 current records", n, err)
	}
	if n, _, err := r.Count(ctx, records.Filter{IncludeSuperseded: true}, 100); err != nil || n != 12 {
		t.Errorf("count with superseded = %d, %v; want 12", n, err)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Fatalf("verify = %+v, %v", rep, err)
	}
	man, err := c.Manifest()
	if err != nil || len(man) == 0 {
		t.Fatalf("manifest = %v, %v", man, err)
	}
	extra, _ := recordstest.TextRecords(man[0].ID)
	recordstest.Ingest(t, c, records.Parser{Name: "after-reindex", Version: "1"}, []string{man[0].ID}, extra[:3])
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, `"cafe"`); len(got) != 4 {
		t.Errorf("after a new ingest cafe is found in %v, want 4 records", got)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Errorf("verify after the new ingest = %+v, %v", rep, err)
	}
}

// TestReindexUpgradesStaleVersion: an index built by another version (a changed normalization or
// library) is rebuilt and becomes current.
func TestReindexUpgradesStaleVersion(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	recordstest.SetFTSNormVersion(t, c.Dir, "fts0/unicode-14.0.0/sqlite-3.40.0")
	if st, err := c.IndexState(ctx); err != nil || st.Kind != evidence.IndexStale {
		t.Fatalf("state = %+v, %v; want stale", st, err)
	}
	res, err := c.ReindexText(ctx, evidence.ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromNormVersion != "fts0/unicode-14.0.0/sqlite-3.40.0" || res.NormVersion != evidence.FTSNormVersion() {
		t.Errorf("result = %+v", res)
	}
	if st, err := c.IndexState(ctx); err != nil || st.Kind != evidence.IndexCurrent {
		t.Fatalf("state after = %+v, %v; want current", st, err)
	}
	checkCorpusSearches(t, c, "after the reindex")
}

// TestReindexRepairsEveryIndexState: a reindex starts from any state, including one that cannot be
// classified (a garbage value, a missing key) and leaves the key holding the current version.
func TestReindexRepairsEveryIndexState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *evidence.Case)
		from  string
	}{
		{"unbuilt", func(t *testing.T, c *evidence.Case) { recordstest.SetFTSNormVersion(t, c.Dir, "") }, ""},
		{"building", func(t *testing.T, c *evidence.Case) { recordstest.SetFTSNormVersion(t, c.Dir, "building") }, "building"},
		{"garbage", func(t *testing.T, c *evidence.Case) { recordstest.SetFTSNormVersion(t, c.Dir, "garbage") }, "garbage"},
		{"missing key", func(t *testing.T, c *evidence.Case) {
			db, err := sql.Open("sqlite", filepath.Join(c.Dir, "artifacts.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`DELETE FROM records_meta WHERE key = 'fts_norm_version'`); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := corpusIndexedCase(t)
			tc.setup(t, c)
			if err := c.RequireIndexCurrent(ctx); !errors.Is(err, evidence.ErrIndexNotCurrent) {
				t.Fatalf("the scenario needs a refused index: %v", err)
			}
			res, err := c.ReindexText(ctx, evidence.ReindexOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if res.FromNormVersion != tc.from {
				t.Errorf("FromNormVersion = %q, want %q", res.FromNormVersion, tc.from)
			}
			if err := c.RequireIndexCurrent(ctx); err != nil {
				t.Fatal(err)
			}
			checkCorpusSearches(t, c, "after the reindex")
			if rep, err := c.Verify(); err != nil || !rep.OK() || len(rep.Notices) != 0 {
				t.Errorf("verify = %+v, %v", rep, err)
			}
		})
	}
}

// TestReindexRefusedWhileIngestActive: a started writer holds the live-ingest slot; the reindex is
// refused with ErrIngestActive before anything is audited or touched, and runs once the ingest ends.
func TestReindexRefusedWhileIngestActive(t *testing.T) {
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	w := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
	add(t, w, recs[:4])
	before := len(auditOf(t, c, ""))
	_, err := c.ReindexText(ctx, evidence.ReindexOptions{})
	if !errors.Is(err, records.ErrIngestActive) {
		t.Fatalf("ReindexText = %v, want ErrIngestActive", err)
	}
	if !strings.Contains(err.Error(), w.IngestID()) {
		t.Errorf("the refusal %q does not name the live ingest %s", err, w.IngestID())
	}
	if got := len(auditOf(t, c, "")); got != before {
		t.Errorf("the refusal audited %d entries", got-before)
	}
	if got := scalar[string](t, c, `SELECT value FROM records_meta WHERE key = 'fts_norm_version'`); got != evidence.FTSNormVersion() {
		t.Errorf("the state is %q", got)
	}
	add(t, w, recs[4:])
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatalf("ReindexText after the ingest ended: %v", err)
	}
}

// TestReindexRefusesTamperedSchema: a case whose schema objects are not the ones this build defines
// (an FTS table with another definition, a missing shadow table, a planted object) is refused with
// ErrIntegrity before anything is audited or dropped: reindex never builds on, or papers over, a
// tampered schema.
func TestReindexRefusesTamperedSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, dir string)
	}{
		{"FTS table definition altered", func(t *testing.T, dir string) {
			def := recordstest.SchemaSQL(t, dir, "table", evidence.FTSWordTable)
			recordstest.RedefineSchemaObject(t, dir, "table", evidence.FTSWordTable, replaceOnce(t, def, "remove_diacritics 2", "remove_diacritics 0"))
		}},
		{"shadow table dropped", func(t *testing.T, dir string) { recordstest.DropTable(t, dir, "records_fts_sub_data") }},
		{"table planted", func(t *testing.T, dir string) {
			recordstest.CreateSchemaObject(t, dir, `CREATE TABLE records_fts_old (x TEXT)`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := corpusIndexedCase(t)
			tc.tamper(t, c.Dir)
			before := len(auditOf(t, c, ""))
			_, err := c.ReindexText(ctx, evidence.ReindexOptions{})
			if !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("ReindexText = %v, want ErrIntegrity", err)
			}
			if got := len(auditOf(t, c, "")); got != before {
				t.Errorf("the refusal audited %d entries", got-before)
			}
			if got := scalar[string](t, c, `SELECT value FROM records_meta WHERE key = 'fts_norm_version'`); got != evidence.FTSNormVersion() {
				t.Errorf("the state is %q: the refused reindex changed it", got)
			}
		})
	}
}

// TestReindexRecoversCorruptIndexStructure: a wrecked index structure record (reads and writes of the
// index fail inside SQLite) does not stop a reindex, because it drops and recreates the tables; the
// schema afterwards is the one of a fresh case.
func TestReindexRecoversCorruptIndexStructure(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	recordstest.WreckFTSStructure(t, c.Dir, evidence.FTSWordTable)
	recordstest.WreckFTSStructure(t, c.Dir, evidence.FTSSubTable)
	if err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.Query(`SELECT rowid FROM records_fts WHERE records_fts MATCH '"cafe"'`)
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
			}
			err = rows.Err()
		}
		if err == nil {
			t.Error("the scenario needs a wrecked index that fails to read")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := c.ReindexText(ctx, evidence.ReindexOptions{})
	if err != nil {
		t.Fatalf("ReindexText over a wrecked index: %v", err)
	}
	if res.Indexed != corpusIndexed {
		t.Errorf("result = %+v", res)
	}
	checkCorpusSearches(t, c, "after the reindex")
	fresh := recordstest.NewCase(t)
	if got, want := schemaRows(t, c), schemaRows(t, fresh); !slices.Equal(got, want) {
		t.Errorf("sqlite_master differs from a fresh case:\n%s\n--- want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Errorf("verify = %+v, %v", rep, err)
	}
}

// TestReindexIsRepeatable: reindexing again and again gives the identical index content (vocab,
// docsize, config, totals) as the writer's build, and two audit pairs for two runs.
func TestReindexIsRepeatable(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	built := indexContent(t, c)
	before := len(auditOf(t, c, ""))
	var ids []string
	for i := range 2 {
		res, err := c.ReindexText(ctx, evidence.ReindexOptions{ChunkRows: 7})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ReindexID)
		if got := indexContent(t, c); got != built {
			t.Errorf("run %d: the index content differs from the first build", i+1)
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("two reindex runs share the id %s", ids[0])
	}
	var actions []string
	for _, e := range auditOf(t, c, "")[before:] {
		actions = append(actions, e.Action)
	}
	want := []string{evidence.ActionReindex, evidence.ActionReindexDone, evidence.ActionReindex, evidence.ActionReindexDone}
	if !slices.Equal(actions, want) {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}
