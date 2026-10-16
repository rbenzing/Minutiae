package records_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestOverviewStatementsUseIndexes(t *testing.T) {
	c, _ := bigDataset(t, 300) // no supersession
	qs, err := records.OverviewQueries(records.Filter{}, false)
	if err != nil || len(qs) != 6 {
		t.Fatalf("%d statements, %v", len(qs), err)
	}
	for i, q := range qs {
		plan := explain(t, c, q.SQL, q.Args)
		if !strings.Contains(plan, "INDEX") || strings.Contains(plan, "USE TEMP B-TREE") {
			t.Errorf("statement %d is not served by an index:\n%s\n%s", i, q.SQL, plan)
		}
	}
}

// The six statements answer exactly what the old single scan answered.
func TestOverviewAnswersEqualTheOldSingleScan(t *testing.T) {
	c, r := bigDataset(t, 300)
	for _, tc := range []struct {
		name  string
		f     records.Filter
		where string
	}{
		{"all", records.Filter{}, ""},
		{"one type", records.Filter{Types: []string{"message"}}, " WHERE r.type = 'message'"},
		{"deleted only", records.Filter{Deleted: records.Only}, " WHERE r.deleted = 1"},
		{"none deleted", records.Filter{Deleted: records.None}, " WHERE r.deleted = 0"},
	} {
		got, err := r.Overview(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		var n, del, rec, untimed int64
		var lo, hi sql.NullInt64
		err = c.ReadTx(ctx, func(h evidence.ReadHandle) error {
			return h.QueryRow("SELECT count(*), COALESCE(sum(r.deleted), 0), COALESCE(sum(r.recovered), 0), COALESCE(sum(r.ts IS NULL), 0), min(r.ts), max(r.ts) FROM records r"+tc.where).
				Scan(&n, &del, &rec, &untimed, &lo, &hi)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Records != n || got.Deleted != del || got.Recovered != rec || got.Untimed != untimed {
			t.Errorf("%s: %+v, want %d %d %d %d", tc.name, got, n, del, rec, untimed)
		}
		if (got.TSMin == nil) != !lo.Valid || (got.TSMin != nil && *got.TSMin != lo.Int64) ||
			(got.TSMax == nil) != !hi.Valid || (got.TSMax != nil && *got.TSMax != hi.Int64) {
			t.Errorf("%s: time range %v..%v, want %v..%v", tc.name, got.TSMin, got.TSMax, lo, hi)
		}
	}
}

func TestIndexStatusDocumentCountsEqualTheDocsizeScan(t *testing.T) {
	c, r := bigDataset(t, 300)
	st, err := r.IndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w, s := count(t, c, "records_fts_docsize"), count(t, c, "records_fts_sub_docsize"); int(st.WordDocs) != w || int(st.SubstringDocs) != s {
		t.Fatalf("index status %d/%d, docsize rows %d/%d", st.WordDocs, st.SubstringDocs, w, s)
	}
}

func TestIndexStatusRefusesAGarbageTotalsRecord(t *testing.T) {
	c, _ := bigDataset(t, 50)
	recordstest.SetFTSDataBlock(t, c.Dir, "records_fts", 1, []byte{0x81})
	r := newReader(t, c)
	if _, err := r.IndexStatus(ctx); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("a garbage totals record must be an integrity error, never a zero: %v", err)
	}
}

// A totals record that decodes to zero while documents exist is an integrity error, never "0 documents".
func TestIndexStatusRefusesAZeroTotalsRecordWhileDocumentsExist(t *testing.T) {
	c, _ := bigDataset(t, 50)
	recordstest.SetFTSTotals(t, c.Dir, "records_fts", []byte{0x00})
	r := newReader(t, c)
	if st, err := r.IndexStatus(ctx); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("a zero totals record with documents must be an integrity error, got %+v, %v", st, err)
	}
}

// An empty totals block while documents exist is an integrity error too.
func TestIndexStatusRefusesAnEmptyTotalsRecordWhileDocumentsExist(t *testing.T) {
	c, _ := bigDataset(t, 50)
	recordstest.SetFTSTotals(t, c.Dir, "records_fts_sub", []byte{})
	r := newReader(t, c)
	if st, err := r.IndexStatus(ctx); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("an empty totals record with documents must be an integrity error, got %+v, %v", st, err)
	}
}
