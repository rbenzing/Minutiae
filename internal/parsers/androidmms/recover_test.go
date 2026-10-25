package androidmms_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold history in the claimed tables, with a recover oracle.
var recoverVariants = []string{"av-freelist-512", "wal-uncheckpointed", "hot-journal", "mmssms-pdu-deleted"}

// Spec section 14: the claimed tables map their recovered rows, checked against an oracle that was computed by
// an independent page walker from the committed files and the scenario's own record of what it wrote.
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range recoverVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remotePath)
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidmms.Parser{}, b, parsertest.LoadRecoverExpect(t, "testdata", v))
		})
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind (plan 4G Task 9 step 1).
func TestRecoverOracleShape(t *testing.T) {
	type key struct{ table, method, relation, basis string }
	count := func(v string) map[key]int {
		m := map[key]int{}
		for _, r := range parsertest.LoadRecoverExpect(t, "testdata", v).Records {
			m[key{r.Table, r.Method, r.Relation, r.Basis}]++
		}
		return m
	}
	if n := count("av-freelist-512")[key{"sms", "sqlite-freelist", "unknown", "fit"}]; n < 100 {
		t.Errorf("av-freelist-512 lists %d recovered sms rows from the freelist, want at least 100", n)
	}
	for _, r := range parsertest.LoadRecoverExpect(t, "testdata", "av-freelist-512").Records {
		if r.Confidence != 60 || len(r.Notes) != 1 || r.Notes[0] != "identity-by-fit-only" {
			t.Fatalf("freelist row %+v: want confidence 60 and the note identity-by-fit-only", r)
		}
	}
	if n := count("wal-uncheckpointed")[key{"sms", "sqlite-wal-prior", "superseded-version", "schema"}]; n == 0 {
		t.Error("wal-uncheckpointed lists no superseded sms version")
	}
	for _, r := range parsertest.LoadRecoverExpect(t, "testdata", "wal-uncheckpointed").Records {
		if r.Rowid == nil || *r.Rowid != 2 || r.Confidence != 75 || r.Record.Body == nil || !strings.Contains(*r.Record.Body, "hi there") {
			t.Errorf("wal-uncheckpointed lists %+v, want only the pre-edit _id 2 at 75", r)
		}
	}
	if n := count("hot-journal")[key{"sms", "sqlite-journal-rolledback", "uncommitted", "schema"}]; n < 20 {
		t.Errorf("hot-journal lists %d interrupted versions, want at least 20", n)
	}
	for _, r := range parsertest.LoadRecoverExpect(t, "testdata", "hot-journal").Records {
		if r.Confidence != 45 || r.Record.Body == nil || !strings.HasPrefix(*r.Record.Body, "interrupted-") {
			t.Fatalf("hot-journal row %+v: want an interrupted version at 45", r)
		}
	}
	pdu := 0
	for _, r := range parsertest.LoadRecoverExpect(t, "testdata", "mmssms-pdu-deleted").Records {
		if r.Table != "pdu" {
			continue
		}
		pdu++
		if r.Record.Payload["participants_unknown"] != true {
			t.Errorf("deleted pdu %v: its addr rows are gone from live, so participants_unknown must be true: %v", *r.Rowid, r.Record.Payload)
		}
	}
	if pdu == 0 {
		t.Error("mmssms-pdu-deleted lists no deleted pdu row")
	}
}

func TestEveryRecoverOracleHasAFixture(t *testing.T) {
	for _, v := range recoverVariants {
		if !slices.Contains(variants, v) {
			t.Errorf("%s has a recover oracle but is not a fixture variant", v)
		}
		parsertest.LoadRecoverExpect(t, "testdata", v) // also checks it was computed from the committed files
	}
}

// Review Focus 3: a recovered row with a lost rowid is reported, never mapped through an invented id.
func TestRecoveredRowWithLostRowidIsUnmappedNotDropped(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "mmssms-pdu-deleted").Bundle(h, remotePath)
	rr, ok := parsertest.FirstClaimedRow(t, h, androidmms.Parser{}, b, "pdu", nil)
	if !ok {
		t.Fatal("the fixture delivers no pdu row to the typed pass")
	}
	if res := parsertest.MapRecoveredRow(t, h, androidmms.Parser{}, b, rr); res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
		t.Fatalf("the intact row: %+v, want one record", res)
	}
	rr.Rowid = nil
	res := parsertest.MapRecoveredRow(t, h, androidmms.Parser{}, b, rr)
	if res.Err != nil || len(res.Records) != 0 || !strings.Contains(res.Unmapped, "no rowid") {
		t.Fatalf("the row without a rowid: %+v, want it unmapped with a reason naming the missing rowid", res)
	}
}

// Review Focus 3, C6: clipped text is marked unknown, never padded and never taken for the whole value.
func TestRecoveredClippedBodyIsMarkedNotRepaired(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "av-freelist-512").Bundle(h, remotePath)
	var bodyAt int
	rr, ok := parsertest.FirstClaimedRow(t, h, androidmms.Parser{}, b, "sms", func(r sqlitefile.RecoveredRow) bool {
		for i, v := range r.Values { // the body: the first long text of the row
			if v.Kind == sqlitefile.KindText && len(v.Bytes) > 40 {
				bodyAt = i
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatal("the fixture delivers no sms row with a long body")
	}
	whole := parsertest.MapRecoveredRow(t, h, androidmms.Parser{}, b, rr)
	if len(whole.Records) != 1 || whole.Records[0].Body == "" {
		t.Fatalf("the intact row: %+v, want a record with its body", whole)
	}
	v := &rr.Values[bodyAt]
	v.Clipped, v.Bytes = true, v.Bytes[:8]
	res := parsertest.MapRecoveredRow(t, h, androidmms.Parser{}, b, rr)
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("the clipped row: %+v, want one record", res)
	}
	rec := res.Records[0]
	if rec.Body != "" {
		t.Errorf("a clipped body came back as text %q: it must be unknown, not repaired", rec.Body)
	}
	raw, _ := rec.Payload["raw"].(map[string]any)
	unk, _ := raw["unknown_columns"].(map[string]any)
	if unk["body"] != "clipped" {
		t.Errorf("raw.unknown_columns = %v, want body marked clipped", unk)
	}
	if _, has := rec.Payload["text_source"]; has {
		t.Errorf("a record without a body states a text source: %v", rec.Payload)
	}
}

// Review Focus 3 (decoy, 4G-R23): the rows of a dropped table of the sms shape are attributed to sms by a
// strict fit alone; the oracle states it and the typed pass reproduces exactly that, marked fit.
func TestFitDecoyTypedRecordIsMarkedFitOnly(t *testing.T) {
	want := parsertest.LoadRecoverExpect(t, "testdata", "mmssms-pdu-deleted")
	decoys := 0
	for _, r := range want.Records {
		if r.Record.Body == nil || !strings.HasPrefix(*r.Record.Body, "decoy ") {
			continue
		}
		decoys++
		if r.Table != "sms" || r.Basis != "fit" || r.Confidence > 60 || !slices.Contains(r.Notes, "identity-by-fit-only") {
			t.Errorf("decoy row %v: table %q basis %q confidence %d notes %v, want a fit row attributed to sms, at most 60, noted identity-by-fit-only",
				*r.Rowid, r.Table, r.Basis, r.Confidence, r.Notes)
		}
	}
	if decoys < 10 {
		t.Fatalf("the oracle lists %d decoy rows, want at least 10 (the misattribution is the point of the fixture)", decoys)
	}
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "mmssms-pdu-deleted").Bundle(h, remotePath)
	parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidmms.Parser{}, b, want)
}

// wrongMapper is android.mmssms with a deliberate fault in what its mappings make of a recovered row.
type wrongMapper struct {
	androidmms.Parser
	fault string
}

func (w wrongMapper) Mappings() []parse.TableMapping {
	ms := w.Parser.Mappings()
	for i := range ms {
		inner := ms[i].Map
		ms[i].Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			recs, err := inner(m, row)
			if err != nil || len(recs) == 0 {
				return recs, err
			}
			switch w.fault {
			case "drops-body":
				recs[0].Body = ""
			case "adds-record":
				recs = append(recs, recs[0])
			case "sets-recovery":
				recs[0].Recovery = "sqlite-freelist"
			}
			return recs, nil
		}
	}
	return ms
}

type failureRecorder struct {
	testing.TB
	errs []string
}

func (r *failureRecorder) Errorf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }

func (r *failureRecorder) Fatalf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	for _, tc := range []struct{ fault, want string }{
		{"drops-body", "Body"},
		{"adds-record", "typed records for the cell"},
		{"sets-recovery", "only the host stamps"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", "wal-uncheckpointed").Bundle(h, remotePath)
			rec := &failureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, wrongMapper{fault: tc.fault}, b, parsertest.LoadRecoverExpect(t, "testdata", "wal-uncheckpointed"))
			if len(rec.errs) == 0 || !strings.Contains(strings.Join(rec.errs, "\n"), tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.errs)
			}
		})
	}
	// and the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "wal-uncheckpointed").Bundle(h, remotePath)
	rec := &failureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, wrongMapper{}, b, parsertest.LoadRecoverExpect(t, "testdata", "wal-uncheckpointed"))
	if len(rec.errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.errs)
	}
}
