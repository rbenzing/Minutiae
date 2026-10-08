package evidence

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestV3StatementsAreTheSharedDDL (R20): the migration creates exactly the tables the shared DDL
// functions name (reindex and verify use the same text), and nothing else but the meta row.
func TestV3StatementsAreTheSharedDDL(t *testing.T) {
	if got := FTSTables(); !slices.Equal(got, []string{FTSWordTable, FTSSubTable}) {
		t.Fatalf("FTSTables() = %v", got)
	}
	var want []string
	for _, table := range FTSTables() {
		ddl, ok := FTSTableDDL(table)
		if !ok {
			t.Fatalf("FTSTableDDL(%q) is unknown", table)
		}
		vocab, ok := FTSVocabDDL(table)
		if !ok {
			t.Fatalf("FTSVocabDDL(%q) is unknown", table)
		}
		if !strings.HasPrefix(ddl, "CREATE VIRTUAL TABLE "+table+" USING fts5(") {
			t.Errorf("FTSTableDDL(%q) = %q", table, ddl)
		}
		if !strings.HasPrefix(vocab, "CREATE VIRTUAL TABLE "+table+"_v USING fts5vocab("+table+", 'instance')") {
			t.Errorf("FTSVocabDDL(%q) = %q", table, vocab)
		}
		if !slices.Contains(v3Statements, ddl) {
			t.Errorf("v3Statements lacks FTSTableDDL(%q)", table)
		}
		if !slices.Contains(v3Statements, vocab) {
			t.Errorf("v3Statements lacks FTSVocabDDL(%q)", table)
		}
		want = append(want, ddl, vocab)
	}
	for _, s := range v3Statements {
		if !slices.Contains(want, s) && !strings.HasPrefix(s, "INSERT INTO records_meta") {
			t.Errorf("v3Statements holds a statement that is not the shared DDL: %q", s)
		}
	}
	if len(v3Statements) != len(want)+1 {
		t.Errorf("v3Statements holds %d statements, want %d (the DDL and the meta row)", len(v3Statements), len(want)+1)
	}
	for _, bad := range []string{"", "records", "records_fts_v", "records_fts_data", "x; DROP TABLE records"} {
		if _, ok := FTSTableDDL(bad); ok {
			t.Errorf("FTSTableDDL(%q) answered", bad)
		}
		if _, ok := FTSVocabDDL(bad); ok {
			t.Errorf("FTSVocabDDL(%q) answered", bad)
		}
	}
}

// TestMigrationsAreAtomicAcrossVersions: the whole pending chain is one transaction. A v1 database
// with a table in the way of the v3 statements fails 1 -> 3 as a whole: it is still v1, with the v1
// records table and none of the v2 tables the first step created before the failure.
func TestMigrationsAreAtomicAcrossVersions(t *testing.T) {
	db := buildV1DB(t, filepath.Join(t.TempDir(), "a.db"))
	if _, err := db.Exec(`CREATE TABLE records_fts (planted TEXT)`); err != nil {
		t.Fatal(err)
	}
	err := applyMigrations(db, 1, 3)
	if err == nil {
		t.Fatal("applyMigrations(db, 1, 3) succeeded although records_fts is taken")
	}
	var v int
	if err := db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("schema version after the failed chain = %d, %v; want 1", v, err)
	}
	cols := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('records')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols[n] = true
	}
	_ = rows.Close()
	if !cols["data"] || !cols["source_path"] || cols["batch_id"] {
		t.Errorf("records is not the v1 table after the failed chain: columns %v", cols)
	}
	for _, table := range []string{"parsers", "record_batches", "record_times", "record_runs", "record_run_artifacts", "record_superseded", "records_meta", "records_fts_sub", "records_fts_v"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s exists after the failed chain (n=%d, %v)", table, n, err)
		}
	}
	var planted string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'records_fts'`).Scan(&planted); err != nil || !strings.Contains(planted, "planted") {
		t.Errorf("the planted table changed: %q, %v", planted, err)
	}
}

// TestMigrateV2ToV3: a v2 database with rows in every table the migration might touch keeps them
// all; the meta value is empty (unbuilt) when records exist and the current version when none do.
func TestMigrateV2ToV3(t *testing.T) {
	counts := func(db *sql.DB) map[string]int {
		out := map[string]int{}
		for _, table := range []string{"artifacts", "parsers", "record_batches", "records", "record_times", "record_runs"} {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	t.Run("with records", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		s, err := openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if err := applyMigrations(s.db, 0, 2); err != nil {
			t.Fatal(err)
		}
		seedRecordTables(t, s)
		before := counts(s.db)
		if before["records"] != 1 || before["artifacts"] != 1 || before["parsers"] != 1 || before["record_batches"] != 1 {
			t.Fatalf("the seed is not what the test assumes: %v", before)
		}
		if err := applyMigrations(s.db, 2, 3); err != nil {
			t.Fatal(err)
		}
		if after := counts(s.db); !equalCounts(after, before) {
			t.Fatalf("row counts changed by the migration: %v -> %v", before, after)
		}
		var v string
		if err := s.db.QueryRow(`SELECT value FROM records_meta WHERE key = ?`, MetaFTSNormVersion).Scan(&v); err != nil || v != "" {
			t.Fatalf("fts_norm_version = %q, %v; want the empty (unbuilt) value", v, err)
		}
		var next string
		if err := s.db.QueryRow(`SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&next); err != nil || next != "1" {
			t.Fatalf("next_id = %q, %v", next, err)
		}
		var ver int
		if err := s.db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&ver); err != nil || ver != 3 {
			t.Fatalf("version = %d, %v", ver, err)
		}
	})
	t.Run("without records", func(t *testing.T) {
		db := rawDB(t, filepath.Join(t.TempDir(), "b.db"))
		if err := applyMigrations(db, 0, 2); err != nil {
			t.Fatal(err)
		}
		if err := applyMigrations(db, 2, 3); err != nil {
			t.Fatal(err)
		}
		var v string
		if err := db.QueryRow(`SELECT value FROM records_meta WHERE key = ?`, MetaFTSNormVersion).Scan(&v); err != nil || v != FTSNormVersion() {
			t.Fatalf("fts_norm_version = %q, %v; want %q", v, err, FTSNormVersion())
		}
	})
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestReadIndexStateKinds: the kind of every value of records_meta.fts_norm_version (R2: the key
// is the primary key, so a duplicate cannot exist).
func TestReadIndexStateKinds(t *testing.T) {
	c := newTestCase(t)
	cur := FTSNormVersion()
	setMeta := func(v string) {
		t.Helper()
		if _, err := c.store.db.Exec(`UPDATE records_meta SET value = ? WHERE key = ?`, v, MetaFTSNormVersion); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		value string
		want  IndexKind
	}{
		{"current", cur, IndexCurrent},
		{"empty is unbuilt", "", IndexUnbuilt},
		{"building", "building", IndexBuilding},
		{"an old pipeline", "fts0/unicode-14.0.0/sqlite-3.40.0", IndexStale},
		{"another well-formed version", "fts9/unicode-99.0.0/gounicode-99.0.0/xtext-v9.0.0/sqlite-9.9.9", IndexStale},
		{"the shape before the x/text pin", "fts1/unicode-15.0.0/sqlite-3.53.4", IndexStale},
		{"garbage", "garbage", IndexInvalid},
		{"building in other case", "Building", IndexInvalid},
		{"prefix only", "fts1", IndexInvalid},
		{"empty after the slash", "fts1/", IndexInvalid},
		{"no number", "fts/unicode-15.0.0", IndexInvalid},
		{"a space", "fts1/unicode 15", IndexInvalid},
		{"a newline", "fts1/unicode-15\nx", IndexInvalid},
		{"the current version with a suffix", cur + "x", IndexStale},
		{"far too long", "fts1/" + strings.Repeat("a", 5000), IndexInvalid},
		{"a control character", "fts1/\x01", IndexInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMeta(tc.value)
			var st IndexState
			err := c.ReadTx(context.Background(), func(h ReadHandle) error {
				var err error
				st, err = ReadIndexState(context.Background(), h)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if st.Kind != tc.want {
				t.Fatalf("kind of %q = %q, want %q", tc.value, st.Kind, tc.want)
			}
			if st.Value != tc.value || st.Current != cur {
				t.Fatalf("state = %+v, want Value %q and Current %q", st, tc.value, cur)
			}
			via, err := c.IndexState(context.Background())
			if err != nil || via != st {
				t.Fatalf("Case.IndexState = %+v, %v; want %+v", via, err, st)
			}
			err = c.RequireIndexCurrent(context.Background())
			if tc.want == IndexCurrent {
				if err != nil {
					t.Fatalf("RequireIndexCurrent on a current index: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrIndexNotCurrent) {
				t.Fatalf("RequireIndexCurrent = %v, want ErrIndexNotCurrent", err)
			}
			if want := "records reindex --case " + c.Dir; !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
			if inErr := c.ReadTx(context.Background(), func(h ReadHandle) error { return RequireIndexCurrentIn(context.Background(), h) }); !errors.Is(inErr, ErrIndexNotCurrent) {
				t.Errorf("RequireIndexCurrentIn = %v, want ErrIndexNotCurrent", inErr)
			}
		})
	}

	t.Run("missing key", func(t *testing.T) {
		if _, err := c.store.db.Exec(`DELETE FROM records_meta WHERE key = ?`, MetaFTSNormVersion); err != nil {
			t.Fatal(err)
		}
		st, err := c.IndexState(context.Background())
		if err != nil || st.Kind != IndexInvalid {
			t.Fatalf("state with the key missing = %+v, %v; want invalid", st, err)
		}
		if err := c.RequireIndexCurrent(context.Background()); !errors.Is(err, ErrIndexNotCurrent) {
			t.Fatalf("RequireIndexCurrent = %v", err)
		}
	})
}

// TestIndexStateNeedsSchemaV3: the state of an older case is ErrNeedsUpgrade, not "invalid".
func TestIndexStateNeedsSchemaV3(t *testing.T) {
	for _, v := range []int{1, 2} {
		c, err := Open(makeCaseAt(t, v))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.IndexState(context.Background()); !errors.Is(err, ErrNeedsUpgrade) {
			t.Errorf("IndexState on a v%d case = %v, want ErrNeedsUpgrade", v, err)
		}
		if err := c.RequireIndexCurrent(context.Background()); !errors.Is(err, ErrNeedsUpgrade) {
			t.Errorf("RequireIndexCurrent on a v%d case = %v, want ErrNeedsUpgrade", v, err)
		}
		_ = c.Close()
	}
}

// TestVerifyMetaKeysAllowsOnlyKnownKeys: fts_norm_version is a known key of a v3 case; any other key
// is a problem; and on a v2 case the key is not part of the schema (R21).
func TestVerifyMetaKeysAllowsOnlyKnownKeys(t *testing.T) {
	c := newTestCase(t)
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 0 {
		t.Fatalf("a fresh v3 case: %+v", r)
	}
	if _, err := c.store.db.Exec(`INSERT INTO records_meta (key, value) VALUES ('note', 'x')`); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, `records_meta holds the key "note", which is not part of the schema`) || len(r.Problems) != 1 {
		t.Fatalf("an unknown key: %+v", r)
	}

	t.Run("v2 case holds the key", func(t *testing.T) {
		c2, err := Open(makeCaseAt(t, 2))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c2.Close() }()
		if r := mustVerify(t, c2); !r.OK() {
			t.Fatalf("a v2 case: %+v", r)
		}
		if _, err := c2.store.db.Exec(`INSERT INTO records_meta (key, value) VALUES ('fts_norm_version', '')`); err != nil {
			t.Fatal(err)
		}
		r := mustVerify(t, c2)
		if r.OK() || !containsSubstr(r.Problems, `records_meta holds the key "fts_norm_version", which is not part of the schema`) {
			t.Fatalf("fts_norm_version in a v2 case: %+v", r)
		}
	})
}

// TestVerifyReportsIndexState: the index states of the plan's table. A current index is silent here
// (P11 compares its content in a later task); every other state is a NOTICE naming the remedy, and
// an invalid one is a problem.
func TestVerifyReportsIndexState(t *testing.T) {
	c := newTestCase(t)
	seedRecordTables(t, c.store)
	setMeta := func(v string) {
		t.Helper()
		if _, err := c.store.db.Exec(`UPDATE records_meta SET value = ? WHERE key = ?`, v, MetaFTSNormVersion); err != nil {
			t.Fatal(err)
		}
	}
	remedy := "records reindex --case " + c.Dir
	notices := func(v string) []string {
		setMeta(v)
		var out []string
		for _, n := range mustVerify(t, c).Notices {
			if strings.Contains(n, "records_fts") {
				out = append(out, n)
			}
		}
		return out
	}
	if got := notices(FTSNormVersion()); len(got) != 0 {
		t.Errorf("a current index raised notices %v", got)
	}
	got := notices("")
	if len(got) != 1 || !strings.Contains(got[0], remedy) || !strings.Contains(got[0], "1 record") || !strings.Contains(got[0], "not searchable") {
		t.Errorf("unbuilt: %v", got)
	}
	got = notices("building")
	if len(got) != 1 || !strings.Contains(got[0], remedy) || !strings.Contains(got[0], "interrupted") {
		t.Errorf("building: %v", got)
	}
	got = notices("fts0/old")
	if len(got) != 1 || !strings.Contains(got[0], remedy) || !strings.Contains(got[0], `"fts0/old"`) || !strings.Contains(got[0], FTSNormVersion()) {
		t.Errorf("stale: %v", got)
	}
	setMeta("garbage")
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, `records_fts: records_meta fts_norm_version holds "garbage"`) {
		t.Errorf("invalid: problems %v", r.Problems)
	}
	if _, err := c.store.db.Exec(`DELETE FROM records_meta WHERE key = ?`, MetaFTSNormVersion); err != nil {
		t.Fatal(err)
	}
	r = mustVerify(t, c)
	if !containsSubstr(r.Problems, "fts_norm_version is missing") {
		t.Errorf("missing key: problems %v", r.Problems)
	}
}

// TestUpgradeResumesThenContinues: a chain that was announced as 1 -> 2 (an older build) and
// committed, with no conclusion, is concluded first (resumed) and the upgrade goes on to v3; the
// audit log tells both steps and verify is clean.
func TestUpgradeResumesThenContinues(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.Audit.Append(ActionCaseUpgrade, "", upgradeDetails(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := c.store.migrateTo(2); err != nil {
		t.Fatal(err)
	}
	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if res != (UpgradeResult{From: 2, To: 3, Upgraded: true, Resumed: true, ResumedFrom: 1, ResumedTo: 2}) {
		t.Fatalf("result = %+v", res)
	}
	var steps []string
	for _, e := range auditEntries(t, c) {
		if strings.HasPrefix(e.Action, "case.upgrade") {
			g := detailInts(t, e, "from", "to")
			steps = append(steps, e.Action+" "+string(rune('0'+g[0]))+">"+string(rune('0'+g[1])))
		}
	}
	want := []string{"case.upgrade 1>2", "case.upgrade.done 1>2", "case.upgrade 2>3", "case.upgrade.done 2>3"}
	if !slices.Equal(steps, want) {
		t.Fatalf("upgrade audit = %v, want %v", steps, want)
	}
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 0 {
		t.Fatalf("verify = %+v", r)
	}
}

// TestIndexStateRefusesTamperedSchema: the index state is read through the schema guard like every
// other record read, so a case whose FTS table definition was rewritten (writable_schema: the rows
// and shadow tables stay, the tokenizer now means something else) is refused with ErrIntegrity, not
// answered, and verify names the table. No trigger is involved: the immutability triggers are
// untouched and the schema text of records_fts is the only difference.
func TestIndexStateRefusesTamperedSchema(t *testing.T) {
	c := newTestCase(t)
	db := rawDB(t, filepath.Join(c.Dir, dbFile))
	var def string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, FTSWordTable).Scan(&def); err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(def, "remove_diacritics 2", "remove_diacritics 0", 1)
	if altered == def {
		t.Fatalf("the definition %q has no remove_diacritics 2 to alter", def)
	}
	if _, err := db.Exec(`PRAGMA writable_schema = ON`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE sqlite_master SET sql = ? WHERE type = 'table' AND name = ?`, altered, FTSWordTable)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%d schema rows changed", n)
	}

	ctx := context.Background()
	if st, err := c.IndexState(ctx); !errors.Is(err, ErrIntegrity) {
		t.Errorf("IndexState = %+v, %v; want ErrIntegrity", st, err)
	} else if !strings.Contains(err.Error(), `table "records_fts" was altered`) {
		t.Errorf("the refusal does not name the altered table: %v", err)
	}
	if err := c.RequireIndexCurrent(ctx); !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrIndexNotCurrent) {
		t.Errorf("RequireIndexCurrent = %v; want ErrIntegrity (and not a reindex hint)", err)
	}
	if err := c.RequireSchemaObjects(ctx); !errors.Is(err, ErrIntegrity) {
		t.Errorf("RequireSchemaObjects = %v; want ErrIntegrity", err)
	}
	rep := mustVerify(t, c)
	if rep.OK() || !containsSubstr(rep.Problems, `table "records_fts" was altered`) {
		t.Fatalf("verify = %+v; want the problem `table \"records_fts\" was altered`", rep)
	}
}
