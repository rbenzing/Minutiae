package records_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func openCase(t *testing.T, dir string) *evidence.Case {
	t.Helper()
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// listCounts returns the number of current records and of all records (superseded runs included).
func listCounts(t *testing.T, c *evidence.Case) (current, all int) {
	t.Helper()
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, superseded := range []bool{false, true} {
		res, err := r.List(context.Background(), records.Filter{IncludeSuperseded: superseded}, records.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if superseded {
			all = len(res.Rows)
		} else {
			current = len(res.Rows)
		}
	}
	return current, all
}

// TestUpgradeV2CaseWithRecordsLeavesIndexUnbuilt: the upgrade of a v2 case that holds records is
// audited as one {2,3} step, tells how many records have no index, leaves the index unbuilt (never
// silently empty), verifies OK with a notice that names the remedy, and the records stay listable.
func TestUpgradeV2CaseWithRecordsLeavesIndexUnbuilt(t *testing.T) {
	dir := recordstest.CopyV2Case(t)
	c := openCase(t, dir)

	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if want := (evidence.UpgradeResult{From: 2, To: 3, Upgraded: true, RecordsToIndex: 12}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}

	es, err := evidence.ReadAuditEntries(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var tail []evidence.AuditEntry
	for i, e := range es {
		if e.Action == "case.open" {
			tail = es[i+1:]
		}
	}
	var ups []evidence.AuditEntry
	for _, e := range tail {
		if strings.HasPrefix(e.Action, "case.upgrade") {
			ups = append(ups, e)
		}
	}
	if len(ups) != 2 || ups[0].Action != "case.upgrade" || ups[1].Action != "case.upgrade.done" {
		t.Fatalf("upgrade audit entries after case.open = %+v", ups)
	}
	for _, e := range ups {
		if fmt.Sprint(e.Details["from"]) != "2" || fmt.Sprint(e.Details["to"]) != "3" {
			t.Errorf("%s details = %v, want from 2 to 3", e.Action, e.Details)
		}
	}

	st, err := c.IndexState(context.Background())
	if err != nil || st.Kind != evidence.IndexUnbuilt || st.Value != "" || st.Current != evidence.FTSNormVersion() {
		t.Fatalf("index state = %+v, %v; want unbuilt", st, err)
	}
	if err := c.RequireIndexCurrent(context.Background()); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Fatalf("RequireIndexCurrent = %v, want ErrIndexNotCurrent", err)
	}

	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.RecordsChecked != 12 {
		t.Fatalf("verify: ok=%v records=%d problems=%v", rep.OK(), rep.RecordsChecked, rep.Problems)
	}
	found := false
	for _, n := range rep.Notices {
		if strings.Contains(n, "records reindex --case "+dir) && strings.Contains(n, "12 records") {
			found = true
		}
	}
	if !found {
		t.Fatalf("verify notices %q do not name `records reindex` and the 12 records", rep.Notices)
	}

	if cur, all := listCounts(t, c); cur != 6 || all != 12 {
		t.Fatalf("records list shows %d current and %d total records, want 6 and 12", cur, all)
	}
}

// TestUpgradeEmptyCaseIsCurrent: with no records there is nothing to index, so the index is current
// at once and the upgrade tells nothing about reindexing. (A v1 case has no records by the upgrade's
// own rule; a freshly created case is checked as well.)
func TestUpgradeEmptyCaseIsCurrent(t *testing.T) {
	c := openCase(t, recordstest.NewV1Case(t))
	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if want := (evidence.UpgradeResult{From: 1, To: 3, Upgraded: true}); res != want {
		t.Fatalf("result = %+v, want %+v (no records to index)", res, want)
	}
	if st, err := c.IndexState(context.Background()); err != nil || st.Kind != evidence.IndexCurrent || st.Value != evidence.FTSNormVersion() {
		t.Fatalf("index state = %+v, %v; want current", st, err)
	}
	if err := c.RequireIndexCurrent(context.Background()); err != nil {
		t.Fatalf("RequireIndexCurrent = %v", err)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Fatalf("verify = %+v, %v", rep, err)
	}

	fresh, err := evidence.Create(filepath.Join(t.TempDir(), "FRESH"), evidence.CreateOptions{ID: "FRESH", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if st, err := fresh.IndexState(context.Background()); err != nil || st.Kind != evidence.IndexCurrent {
		t.Fatalf("a new case: index state = %+v, %v; want current", st, err)
	}
	if res, err := fresh.Upgrade(); err != nil || res.RecordsToIndex != 0 || res.Upgraded {
		t.Fatalf("Upgrade of a current case = %+v, %v", res, err)
	}
}

// TestV2CaseStaysV2UnderThisBuild: opening, verifying and listing a v2 case works and changes
// nothing; writing needs the upgrade (nothing migrates implicitly).
func TestV2CaseStaysV2UnderThisBuild(t *testing.T) {
	dir := recordstest.CopyV2Case(t)
	c := openCase(t, dir)
	if v, err := c.SchemaVersion(); err != nil || v != 2 {
		t.Fatalf("schema version = %d, %v; want 2", v, err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.RecordsChecked != 12 || len(rep.Notices) != 0 {
		t.Fatalf("verify: %+v", rep)
	}
	if cur, all := listCounts(t, c); cur != 6 || all != 12 {
		t.Fatalf("records list shows %d current and %d total records, want 6 and 12", cur, all)
	}
	if _, err := records.NewWriter(c, testParser, records.WriterOptions{}); !errors.Is(err, evidence.ErrNeedsUpgrade) {
		t.Fatalf("NewWriter on a v2 case = %v, want ErrNeedsUpgrade", err)
	}
	if _, err := c.IndexState(context.Background()); !errors.Is(err, evidence.ErrNeedsUpgrade) {
		t.Fatalf("IndexState on a v2 case = %v, want ErrNeedsUpgrade", err)
	}
	if v, err := c.SchemaVersion(); err != nil || v != 2 {
		t.Fatalf("schema version after the checks = %d, %v; want 2", v, err)
	}
}
