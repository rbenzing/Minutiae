package records_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func mustVerify(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// expectProblems requires every substring in want to appear in some problem and
// every problem to contain one of want or allowed: a tamper test must leave the
// problem under test as the only kind of problem.
func expectProblems(t *testing.T, rep evidence.VerifyReport, want []string, allowed ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, p := range rep.Problems {
			if strings.Contains(p, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem contains %q; problems: %q", w, rep.Problems)
		}
	}
	for _, p := range rep.Problems {
		ok := false
		for _, w := range append(append([]string{}, want...), allowed...) {
			if strings.Contains(p, w) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	if rep.OK() {
		t.Error("verify reported OK")
	}
}

// ingested is a case with one 1 MiB artifact and one ingest of 30 records in
// three batches of 10 (ids 1..30).
type ingested struct {
	c   *evidence.Case
	art evidence.ManifestRecord
	res records.IngestResult
}

func ingest30(t *testing.T) ingested {
	t.Helper()
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 10}, a.ID)
	add(t, w, recordstest.Records(a.ID, 30, 7))
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 3 || res.Records != 30 {
		t.Fatalf("result = %+v", res)
	}
	return ingested{c, a, res}
}

func TestVerifyCleanAfterIngest(t *testing.T) {
	g := ingest30(t)
	rep := mustVerify(t, g.c)
	if !rep.OK() || rep.RecordsChecked != 30 || rep.RecordBatchesChecked != 3 || len(rep.Notices) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	b, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(b), `"records_checked":30`) || !strings.Contains(string(b), `"record_batches_checked":3`) {
		t.Errorf("json = %s, %v", b, err)
	}
	// the verify.run entry carries the counts
	runs := auditOf(t, g.c, "verify.run")
	if len(runs) != 1 || runs[0].Details["records_checked"] != json.Number("30") {
		t.Errorf("verify.run = %v", runs)
	}
}

func TestVerifyEmptyRecordsIsClean(t *testing.T) {
	c, _ := setup(t)
	rep := mustVerify(t, c)
	if !rep.OK() || rep.RecordsChecked != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyDetectsAlteredRecord(t *testing.T) {
	g := ingest30(t)
	recordstest.SetRecordSummary(t, g.c.Dir, 15, "altered after the fact")
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"digest mismatch", "a search hit is invented", "a search hit is hidden"}, "records_fts") // the index still holds the old text
	// it names the batch and the id range
	if !strings.Contains(strings.Join(rep.Problems, "\n"), "batch 2 of ingest "+`"`+g.res.IngestID+`"`+" (ids 11..20)") {
		t.Errorf("the problem does not name the batch and its id range: %q", rep.Problems)
	}
}

// fullRecord has every stored column populated.
func fullRecord(artifactID string, i int, basis records.Basis, off int, recovered bool) records.Record {
	t0 := time.Unix(1_650_000_000, 0).UTC()
	tm := records.Time{T: t0, Basis: basis}
	end := records.Time{T: t0.Add(time.Minute), Basis: basis}
	if basis == records.BasisLocalOffset {
		tm.OffsetMin, end.OffsetMin = off, off
	}
	created := records.Time{T: t0.Add(-time.Hour), Basis: basis}
	if basis == records.BasisLocalOffset {
		created.OffsetMin = off
	}
	conf := 80
	rec := records.Record{
		Type: "message", ArtifactID: artifactID, Summary: "full record", Body: "the body",
		SourcePath: "/data/x.db", Locator: "sqlite:t=m;r=" + string(rune('0'+i)),
		Range: &records.Range{Offset: 100, Length: 50}, Time: &tm, TimeEnd: &end,
		Times: []records.NamedTime{
			{Kind: "created", Time: created},
			{Kind: "modified", Time: records.Time{T: t0.Add(time.Hour), Basis: records.BasisLocalUnknown}},
		},
		Deleted: true, Confidence: &conf,
		Payload: map[string]any{"a": 1, "nested": map[string]any{"k": []any{"x", true}}},
	}
	if recovered {
		rec.Recovery = "carve"
	}
	return rec
}

func TestVerifyDetectsDeletedRecord(t *testing.T) {
	g := ingest30(t)
	recordstest.DeleteRecord(t, g.c.Dir, 15)
	expectProblems(t, mustVerify(t, g.c), []string{"records stored", "digest mismatch", "which does not exist or has no indexable text (a search hit is invented)"}, "records_fts")
}

func TestVerifyDetectsErasedBatch(t *testing.T) {
	g := ingest30(t)
	recordstest.DeleteBatch(t, g.c.Dir, g.res.IngestID, 2)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"batch row missing"}, "records_fts") // the index still holds the 10 erased records
	if rep.RecordsChecked != 20 {
		t.Errorf("RecordsChecked = %d, want the 20 that remain", rep.RecordsChecked)
	}
}

func TestVerifyDetectsInjectedRecords(t *testing.T) {
	g := ingest30(t)
	bid := recordstest.InjectBatchRow(t, g.c.Dir, "ing-forged", 1, 31, 2, strings.Repeat("a", 64))
	recordstest.InjectRecord(t, g.c.Dir, 31, bid, g.art.ID, "forged one")
	recordstest.InjectRecord(t, g.c.Dir, 32, bid, g.art.ID, "forged two")
	recordstest.SetNextID(t, g.c, 33)                                                                                                                         // a careful forger fixes the counter too
	expectProblems(t, mustVerify(t, g.c), []string{"not announced", "outside every batch range", "the index lacks", "a search hit is hidden"}, "records_fts") // the forged rows are not indexed
}

func TestVerifyFlagsBatchWithoutAuditCommitment(t *testing.T) {
	g := ingest30(t)
	recordstest.CopyBatchRow(t, g.c.Dir, g.res.IngestID, 1, 4)
	expectProblems(t, mustVerify(t, g.c), []string{"is not announced by any records.batch audit entry"})
}

func TestVerifyDetectsForgedBatchRow(t *testing.T) {
	g := ingest30(t)
	recordstest.SetBatchDigest(t, g.c.Dir, g.res.IngestID, 2, strings.Repeat("0", 64))
	expectProblems(t, mustVerify(t, g.c), []string{"differs from the audit log"})
}

func TestVerifyFlagsRecordOutsideBatchRange(t *testing.T) {
	g := ingest30(t)
	bid := recordstest.BatchID(t, g.c, g.res.IngestID, 3)
	recordstest.InjectRecord(t, g.c.Dir, 50, bid, g.art.ID, "stray")
	recordstest.SetNextID(t, g.c, 51)
	expectProblems(t, mustVerify(t, g.c), []string{"outside every batch range", "a search hit is hidden"}, "records_fts") // the stray row is not indexed
}

func TestVerifyDetectsRepointedRecord(t *testing.T) {
	c, a := setup(t)
	a2 := recordstest.AddArtifact(t, c, "b.db", append([]byte("second"), mib...))
	res := recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 5, 1))
	recordstest.RepointRecord(t, c.Dir, 3, a2.ID)
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"digest mismatch"})
	if !strings.Contains(strings.Join(rep.Problems, "\n"), res.IngestID) {
		t.Errorf("problems do not name the ingest: %q", rep.Problems)
	}
}

func TestVerifyFlagsRecordForMissingArtifact(t *testing.T) {
	t.Run("artifact erased from manifest and db", func(t *testing.T) {
		g := ingest30(t)
		recordstest.RemoveArtifactEverywhere(t, g.c.Dir, g.art.ID)
		rep := mustVerify(t, g.c)
		expectProblems(t, rep, []string{"is not in the manifest"},
			"is not in artifacts.db", "digest mismatch", "artifact.create", "unmanifested file")
		if !strings.Contains(strings.Join(rep.Problems, "\n"), `artifact "`+g.art.ID+`" is not in the manifest`) {
			t.Errorf("the problem does not name the artifact: %q", rep.Problems)
		}
	})
	t.Run("artifact id that matches no artifact at all", func(t *testing.T) {
		g := ingest30(t)
		recordstest.SetRecordColumn(t, g.c.Dir, 4, "artifact_id", "ghost")
		expectProblems(t, mustVerify(t, g.c), []string{`artifact "ghost" is not in the manifest`, `artifact "ghost" is not in artifacts.db`}, "digest mismatch")
	})
}

func TestVerifyDetectsArtifactChangedAfterIngest(t *testing.T) {
	g := ingest30(t)
	forged := append([]byte("forged"), mib[:len(mib)-6]...)
	recordstest.RewriteArtifactConsistently(t, g.c.Dir, g.art.ID, forged)
	expectProblems(t, mustVerify(t, g.c), []string{"digest mismatch", "does not match its artifact.create audit entry"}, "differs from the manifest")
}

func TestVerifySwappedParserID(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, records.Parser{Name: "pa", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 3, 1))
	recordstest.Ingest(t, c, records.Parser{Name: "pb", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 3, 2))
	recordstest.SetRecordParser(t, c.Dir, 1, 2)
	recordstest.SetRecordParser(t, c.Dir, 4, 1)
	expectProblems(t, mustVerify(t, c), []string{"digest mismatch"})
}

func TestVerifyFlagsRecordWithMissingParserOrBatchRow(t *testing.T) {
	g := ingest30(t)
	recordstest.SetRecordParser(t, g.c.Dir, 2, 999)
	expectProblems(t, mustVerify(t, g.c), []string{"parser id 999 does not exist"}, "digest mismatch")

	g = ingest30(t)
	recordstest.SetRecordColumn(t, g.c.Dir, 2, "batch_id", 999)
	expectProblems(t, mustVerify(t, g.c), []string{"batch id 999 does not exist", "is not the batch that announced"})

	g = ingest30(t)
	other := recordstest.BatchID(t, g.c, g.res.IngestID, 3)
	recordstest.SetRecordColumn(t, g.c.Dir, 2, "batch_id", other)
	expectProblems(t, mustVerify(t, g.c), []string{"is not the batch that announced its id"})
}

func TestVerifyDetectsStaleNextID(t *testing.T) {
	g := ingest30(t)
	recordstest.SetNextID(t, g.c, 10)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"next_id"})
}

func TestVerifyDetectsOverlappingBatchRanges(t *testing.T) {
	g := ingest30(t)
	// a validly chained audit entry that announces ids already used by batch 3
	_, err := g.c.Audit.Append(evidence.ActionBatch, "", evidence.BatchCommit{
		IngestID: g.res.IngestID, BatchNo: 4, FirstID: 25, Count: 10, Digest: strings.Repeat("c", 64),
		Artifacts: map[string]string{g.art.ID: g.art.SHA256}, ArtifactIncomplete: []string{}, Types: map[string]int64{"note": 10},
	}.Details())
	if err != nil {
		t.Fatal(err)
	}
	expectProblems(t, mustVerify(t, g.c), []string{"overlaps"},
		"digest mismatch", "records stored", "batch row missing", "outside every batch range", "does not match its audited batches")
}

func TestVerifyFlagsMissingImmutabilityTrigger(t *testing.T) {
	g := ingest30(t)
	recordstest.DropTrigger(t, g.c.Dir, "immut_records_upd")
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{`trigger "immut_records_upd" is missing`})
}

// TestVerifyFlagsDroppedTrigger: a tamper that leaves the triggers dropped.
func TestVerifyFlagsDroppedTrigger(t *testing.T) {
	g := ingest30(t)
	recordstest.DropImmutabilityTriggers(t, g.c.Dir)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"trigger"})
	if len(rep.Problems) != 16 {
		t.Errorf("%d problems for 16 dropped triggers: %q", len(rep.Problems), rep.Problems)
	}
}

// TestVerifyFlagsWeakenedTrigger (R13): a trigger kept under its name whose body
// no longer blocks anything.
func TestVerifyFlagsWeakenedTrigger(t *testing.T) {
	g := ingest30(t)
	recordstest.ReplaceTrigger(t, g.c.Dir, "immut_record_batches_del", "record_batches", "DELETE")
	expectProblems(t, mustVerify(t, g.c), []string{`trigger "immut_record_batches_del" was altered`})
}

func TestVerifyCompletesWhenRecordTablesCorrupt(t *testing.T) {
	t.Run("truncated artifacts.db", func(t *testing.T) {
		g := ingest30(t)
		if err := os.Truncate(filepath.Join(g.c.Dir, "artifacts.db"), 0); err != nil {
			t.Fatal(err)
		}
		rep := mustVerify(t, g.c) // completes, no panic
		if rep.OK() || !strings.Contains(strings.Join(rep.Problems, " | "), "records unreadable") {
			t.Fatalf("problems = %q", rep.Problems)
		}
		es := auditOf(t, g.c, "")
		if es[len(es)-1].Action != "verify.run" {
			t.Errorf("last audit action = %s", es[len(es)-1].Action)
		}
	})
	t.Run("record_times table dropped", func(t *testing.T) {
		g := ingest30(t)
		recordstest.DropTable(t, g.c.Dir, "record_times")
		rep := mustVerify(t, g.c)
		if rep.OK() || !strings.Contains(strings.Join(rep.Problems, "\n"), "records unreadable") {
			t.Fatalf("report = %q", rep.Problems)
		}
	})
}

// TestAcceptedRecordsRoundTripAndVerify: whatever the writer accepts is stored
// exactly as given (read back raw, without the Reader) and verifies OK.
func TestAcceptedRecordsRoundTripAndVerify(t *testing.T) {
	c, a := setup(t)
	r := func(cps ...rune) string { return string(cps) }
	hostile := []string{
		"caf" + r(0xE9),                                // NFC
		"cafe" + r(0x301),                              // NFD of the same text
		"a" + r(0x202E) + "b" + r(0x202C) + "c",        // bidi controls
		"x" + r(0x200B, 0x200D, 0x2060) + "y",          // zero-width characters
		r(0x1F600) + " " + r(0x1F468, 0x200D, 0x1F469), // emoji, a ZWJ sequence
		"  padded\t",
		"line1\r\nline2" + r(0x2028) + "line3",
		r(0xFEFF) + "bom",
		"e" + r(0x301, 0x301, 0x301),
	}
	var recs []records.Record
	for i, s := range hostile {
		recs = append(recs, records.Record{
			Type: "note", ArtifactID: a.ID, Summary: s, SourcePath: "/p/" + s, Body: s + s,
			Locator: "sqlite:" + strings.Repeat("l", 1000) + "#" + string(rune('a'+i)),
			Payload: map[string]any{
				s: s, "nested": map[string]any{"deep": []any{map[string]any{"n": json.Number("9007199254740993")}, json.Number("-0.5e10"), uint64(18446744073709551615), int64(-9223372036854775808)}},
			},
		})
	}
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recs)
	rows := loadRows(t, c)
	if len(rows) != len(recs) {
		t.Fatalf("%d rows for %d records", len(rows), len(recs))
	}
	for i, r := range recs {
		got := rows[i]
		canon, err := records.CanonicalPayload(r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if got.Summary != r.Summary || *got.SourcePath != r.SourcePath || *got.Body != r.Body || *got.Locator != r.Locator || got.Payload != canon {
			t.Errorf("record %d came back changed", i)
		}
		if !strings.Contains(got.Payload, "9007199254740993") {
			t.Errorf("record %d: the big integer was not kept exactly: %s", i, got.Payload)
		}
	}
	rep := mustVerify(t, c)
	if !rep.OK() || rep.RecordsChecked != len(recs) {
		t.Fatalf("report = %+v", rep)
	}
}
