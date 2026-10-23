package evidence_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const verifyCancelledLine = "reproduce: verification cancelled"

// manyBatchCase ingests n runs of 10 records each (n audited batches) and returns the case.
func manyBatchCase(t *testing.T, n int) *evidence.Case {
	t.Helper()
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 11, Artifacts: ids})
	for i := range n {
		recordstest.Ingest(t, c, records.Parser{Name: "loc", Version: fmt.Sprint(i)}, ids, gen.Block(int64(i)*10, 10))
	}
	return c
}

// TestVerifyLocalizationStopsAt64Batches (Review Focus 4): an emptied index over 70 audited batches fails every
// bucket of both tables. The work stays bounded at 64 batches, the listing keeps its 50 items per kind and table,
// and the six batches that were not rebuilt are named, in one line per table, never silently.
func TestVerifyLocalizationStopsAt64Batches(t *testing.T) {
	c := manyBatchCase(t, 70)
	recordstest.EmptyFTSIndex(t, c.Dir)
	var rep evidence.FTSLocalizeReport
	c.SetFTSLocalizeObserver(func(r evidence.FTSLocalizeReport) { rep = r })
	got, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if got.OK() {
		t.Fatal("an emptied index passed")
	}
	if rep.Rebuilt != 64 || rep.Done != 2*64 {
		t.Fatalf("rebuilt %d buckets, %d comparisons; want 64 and 128", rep.Rebuilt, rep.Done)
	}
	for _, table := range evidence.FTSTables() {
		line := table + ": 6 batches whose index postings differ from their records were not localized (at most 64 batches are rebuilt and listed): "
		var found []string
		for _, p := range got.Problems {
			if strings.HasPrefix(p, line) {
				found = append(found, p)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d not-localized lines for %s in %q, want 1", len(found), table, got.Problems)
		}
		for _, ids := range []string{"ids 641..650", "ids 651..660", "ids 661..670", "ids 671..680", "ids 681..690", "ids 691..700"} {
			if !strings.Contains(found[0], ids) {
				t.Errorf("the line does not name the batch of %s (the last ones are not rebuilt): %s", ids, found[0])
			}
		}
		if n := strings.Count(found[0], "batch "); n != 6 {
			t.Errorf("the line names %d batches, want 6: %s", n, found[0])
		}
		hidden := 0
		for _, p := range got.Problems {
			if strings.HasPrefix(p, table+": record ") && strings.Contains(p, "the index lacks") {
				hidden++
			}
		}
		if hidden != 50 {
			t.Errorf("%d hidden lines for %s, want 50", hidden, table)
		}
	}
	if problemsHold(got.Problems, "own rebuild there") {
		t.Errorf("a bucket the localization listed was also taken for a replica mismatch: %q", got.Problems)
	}
	if !problemsHold(got.Problems, "further fts-hidden problems are not listed") || !problemsHold(got.Problems, "further fts-sub-hidden problems are not listed") {
		t.Errorf("the further lines are missing: %q", got.Problems)
	}
}

// TestVerifyLocalizationRoundsRespectThePostingBudget: a round holds as many buckets as fit the posting budget, at
// least one; the problem list does not depend on how the buckets are cut into rounds.
func TestVerifyLocalizationRoundsRespectThePostingBudget(t *testing.T) {
	build := func() *evidence.Case {
		c := ftsCase(t, 15000) // three audited batches of 5000
		for _, id := range []int64{10, 6000, 12000} {
			recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, id)
		}
		return c
	}
	run := func(roundPostings int) ([]string, int) {
		defer evidence.SetFTSLocalizeLimits(0, roundPostings, 0)()
		c := build()
		rebuilds := 0
		c.SetFTSHook(func(point string) {
			if point == "after-rebuild" {
				rebuilds++
			}
		})
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		return rep.Problems, rebuilds
	}
	def, defRounds := run(0)
	one, oneRounds := run(1)
	if defRounds != 1 || oneRounds != 3 {
		t.Fatalf("rounds: %d with the default budget, %d with a budget of 1; want 1 and 3", defRounds, oneRounds)
	}
	if problemsHold(def, "twice") {
		t.Errorf("a posting is listed as indexed twice on an index that holds each once: %q", def)
	}
	if !slices.Equal(def, one) {
		t.Fatalf("the problem lists differ:\ndefault %q\nbudget 1 %q", def, one)
	}
	for _, id := range []int{10, 6000, 12000} {
		if !problemsHold(def, fmt.Sprintf("records_fts: record %d: the index lacks", id)) {
			t.Errorf("record %d is not listed: %q", id, def)
		}
	}
}

// TestVerifyLocalizationFoundCapIsNeverSilent: a round that holds more index rows than the cap is not localized,
// and its buckets are named with the reason.
func TestVerifyLocalizationFoundCapIsNeverSilent(t *testing.T) {
	defer evidence.SetFTSLocalizeLimits(0, 0, 10)()
	c := ftsCase(t, 300)
	recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, 5)
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a tampered index passed")
	}
	const want = "records_fts: 1 batches whose index postings differ from their records were not localized (the batches hold more index rows than the cap of 10 rows per round): batch 1 of ingest "
	if !problemsHold(rep.Problems, want) {
		t.Fatalf("no %q in %q", want, rep.Problems)
	}
	if problemsHold(rep.Problems, "the index lacks") {
		t.Errorf("entries were listed from a round that passed the cap: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d for a comparison that did not run to its end", rep.FTSDocsChecked)
	}
}

// TestVerifyCancelDuringLocalizationKeepsTheFindings (Review Focus 3): a cancel in the middle of the localization
// keeps what the index pass found, adds the cancelled line and names the batches it did not localize; the scratch
// area is gone, the report carries no cancel noise, and the CLI would exit 4 (CancelledOnly is false).
func TestVerifyCancelDuringLocalizationKeepsTheFindings(t *testing.T) {
	c := ftsCase(t, 15000) // three audited batches
	for _, id := range []int64{10, 6000, 12000} {
		recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetFTSHook(func(point string) {
		if point == "after-rebuild" {
			cancel()
		}
	})
	rep, err := c.VerifyWith(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.Problems, verifyCancelledLine) {
		t.Fatalf("no cancelled line in %q", rep.Problems)
	}
	if !problemsHold(rep.Problems, "holds a delete marker for record") {
		t.Errorf("the findings of the index pass were lost: %q", rep.Problems)
	}
	if !problemsHold(rep.Problems, "records_fts: 3 batches whose index postings differ from their records were not localized") {
		t.Errorf("the batches that were not localized are not named: %q", rep.Problems)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "context canceled") || strings.Contains(p, "unreadable") || strings.Contains(p, "could not") {
			t.Errorf("cancel noise in the report: %q", p)
		}
	}
	if rep.CancelledOnly() {
		t.Error("the report holds findings, yet it is only a cancel (the CLI would exit 1, not 4)")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !os.IsNotExist(err) {
		t.Errorf("the scratch area is still there: %v", err)
	}
}

// TestVerifyLocalizationListsDuplicatesAcrossSegments: a record indexed twice (two segments hold the same term
// for it) is listed as a duplicate, and the totals line says the ranking is unreliable.
func TestVerifyLocalizationListsDuplicatesAcrossSegments(t *testing.T) {
	c := ftsCase(t, 300)
	recordstest.DuplicateIndexRow(t, c.Dir, evidence.FTSWordTable, 3)
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !problemsHold(rep.Problems, "records_fts: record 3: the index lists") || !problemsHold(rep.Problems, "twice") {
		t.Fatalf("no duplicate line in %q", rep.Problems)
	}
	if problemsHold(rep.Problems, "own rebuild there") {
		t.Errorf("a duplicate was taken for a replica mismatch: %q", rep.Problems)
	}
	if !problemsHold(rep.Problems, "records_fts: the index totals differ from a rebuild (ranking is unreliable)") {
		t.Fatalf("no totals line in %q", rep.Problems)
	}
}

// TestVerifyLocalizationOutsideBlockSharedWithAnAuditedBatch: a posting under a rowid outside every audited batch
// fails its "outside" block, whose id range also holds the records of an audited batch. Only the stray posting is
// listed: the records of the audited batch belong to another bucket and are not rebuilt for this one.
func TestVerifyLocalizationOutsideBlockSharedWithAnAuditedBatch(t *testing.T) {
	c := ftsCase(t, 300) // batch 1 holds ids 1..300, all in block 0 of ids >> 16
	recordstest.InventIndexText(t, c.Dir, evidence.FTSWordTable, 400, "zzstray", "zzstraybody")
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !problemsHold(rep.Problems, `records_fts: the index holds "zzstray`) {
		t.Fatalf("the stray posting is not listed: %q", rep.Problems)
	}
	if problemsHold(rep.Problems, "the index lacks") {
		t.Errorf("records of an audited batch were taken for the outside block: %q", rep.Problems)
	}
	if problemsHold(rep.Problems, "were not localized") {
		t.Errorf("a bucket was not localized: %q", rep.Problems)
	}
}

// TestVerifyFTSUnreadableIndexPassChecksNoDocument (D153): when the index pass cannot read a table, the comparison
// did not run to its end, so no document is counted as checked.
func TestVerifyFTSUnreadableIndexPassChecksNoDocument(t *testing.T) {
	c := ftsCase(t, 300)
	if rep, err := c.Verify(); err != nil || rep.FTSDocsChecked != 300 {
		t.Fatalf("the clean case: %v, FTSDocsChecked %d", err, rep.FTSDocsChecked)
	}
	recordstest.ReplaceFTSStructure(t, c.Dir, evidence.FTSWordTable, []byte{1, 2, 3})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || !problemsHold(rep.Problems, "records_fts: the index cannot be read") {
		t.Fatalf("an unreadable table was not reported: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d after an index pass that could not read a table", rep.FTSDocsChecked)
	}
}

// TestVerifyCancelBetweenLocalizationRoundsKeepsTheFindings: the same cancel as in the middle of a round, but
// between two rounds (a budget of one posting makes each bucket a round): the cancelled line is there, the
// findings stay, and the buckets of the rounds that did not run are named.
func TestVerifyCancelBetweenLocalizationRoundsKeepsTheFindings(t *testing.T) {
	defer evidence.SetFTSLocalizeLimits(0, 1, 0)()
	c := ftsCase(t, 15000)
	for _, id := range []int64{10, 6000, 12000} {
		recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetFTSHook(func(point string) {
		if point == "compared:records_fts" {
			cancel() // the first round is over
		}
	})
	rep, err := c.VerifyWith(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.Problems, verifyCancelledLine) {
		t.Fatalf("no cancelled line in %q", rep.Problems)
	}
	if !problemsHold(rep.Problems, "records_fts: record 10: the index lacks") || problemsHold(rep.Problems, "records_fts: record 6000: the index lacks") {
		t.Errorf("want the first round's record listed and no later one: %q", rep.Problems)
	}
	if !problemsHold(rep.Problems, "records_fts: 2 batches whose index postings differ from their records were not localized (the verification ended before they were rebuilt)") {
		t.Errorf("the two batches of the rounds that did not run are not named: %q", rep.Problems)
	}
	if rep.CancelledOnly() {
		t.Error("the report holds findings, yet it is only a cancel")
	}
}
