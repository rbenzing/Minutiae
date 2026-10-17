package evidence_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func ftsCase(t *testing.T, n int) *evidence.Case {
	t.Helper()
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 7, Artifacts: ids})
	recordstest.Ingest(t, c, records.Parser{Name: "fts", Version: "1"}, ids, gen.Block(0, n))
	return c
}

func requireProblemContaining(t *testing.T, c *evidence.Case, part string) {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, part) {
			return
		}
	}
	t.Fatalf("no problem contains %q: %q", part, rep.Problems)
}

func TestVerifyFTSStructureCleanCases(t *testing.T) {
	for _, n := range []int{1, 300, 6000} { // 6000 records in batches of 5000: several segments and a merge
		c := ftsCase(t, n)
		rep, err := c.Verify()
		if err != nil || !rep.OK() {
			t.Fatalf("n=%d: %v %q", n, err, rep.Problems)
		}
		if _, err := c.ReindexText(context.Background(), evidence.ReindexOptions{}); err != nil {
			t.Fatal(err)
		}
		if rep, err := c.Verify(); err != nil || !rep.OK() {
			t.Fatalf("n=%d after reindex: %v %q", n, err, rep.Problems)
		}
	}
}

func TestVerifyFlagsUnreferencedFTSRows(t *testing.T) {
	const absent = "is not referenced by the index structure"
	t.Run("A an unreferenced segment in _data", func(t *testing.T) {
		c := ftsCase(t, 300)
		recordstest.InsertFTSShadowRow(t, c.Dir, "records_fts_data", int64(900)<<37|1, []byte("x"))
		requireProblemContaining(t, c, "(segment 900) "+absent)
	})
	t.Run("A the same in the substring index", func(t *testing.T) {
		c := ftsCase(t, 300)
		recordstest.InsertFTSShadowRow(t, c.Dir, "records_fts_sub_data", int64(901)<<37|1, []byte("x"))
		requireProblemContaining(t, c, "(segment 901) "+absent)
	})
	t.Run("B a small id in _data", func(t *testing.T) {
		c := ftsCase(t, 300)
		recordstest.InsertFTSShadowRow(t, c.Dir, "records_fts_data", 77, []byte("x"))
		requireProblemContaining(t, c, "data row 77 ")
	})
	t.Run("B2 a small id when the structure names segment 0", func(t *testing.T) {
		c := ftsCase(t, 300)
		// one level, one segment with id 0: version-1 record, write counter 0
		recordstest.ReplaceFTSStructure(t, c.Dir, "records_fts", []byte{0, 0, 0, 0, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x01})
		recordstest.InsertFTSShadowRow(t, c.Dir, "records_fts_data", 77, []byte("x"))
		requireProblemContaining(t, c, "data row 77 ")
	})
	t.Run("C an unreferenced segment in _idx", func(t *testing.T) {
		c := ftsCase(t, 300)
		recordstest.InsertFTSIdxRow(t, c.Dir, "records_fts_idx", 902, "zz", 1)
		requireProblemContaining(t, c, "segment 902 "+absent)
	})
	t.Run("D the structure of an empty index", func(t *testing.T) {
		c := ftsCase(t, 300)
		scratch, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "e.db")))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = scratch.Close() }()
		ddl, _ := evidence.FTSTableDDL("records_fts")
		if _, err := scratch.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		var empty []byte
		if err := scratch.QueryRow("SELECT block FROM records_fts_data WHERE id = 10").Scan(&empty); err != nil {
			t.Fatal(err)
		}
		recordstest.ReplaceFTSStructure(t, c.Dir, "records_fts", empty)
		requireProblemContaining(t, c, absent)
	})
	t.Run("E a garbage structure record", func(t *testing.T) {
		c := ftsCase(t, 300)
		recordstest.ReplaceFTSStructure(t, c.Dir, "records_fts", []byte{1, 2, 3})
		requireProblemContaining(t, c, "index structure record is unreadable")
	})
}

func TestVerifyListsAtMostFiftyUnreferencedFTSRows(t *testing.T) {
	c := ftsCase(t, 300)
	for i := int64(0); i < 60; i++ {
		recordstest.InsertFTSShadowRow(t, c.Dir, "records_fts_data", (int64(1000)+i)<<37|1, []byte("x"))
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	listed, more := 0, false
	for _, p := range rep.Problems {
		if strings.Contains(p, "is not referenced by the index structure") {
			listed++
		}
		more = more || strings.Contains(p, "further unreferenced index rows are not listed")
	}
	if listed != 50 || !more {
		t.Fatalf("listed %d, summary line %v: %q", listed, more, rep.Problems)
	}
}
