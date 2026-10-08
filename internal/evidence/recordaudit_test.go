package evidence

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func sampleConclusion() IngestConclusion {
	return IngestConclusion{
		IngestID: "ing-1", Outcome: "complete", Batches: 3, Records: 12000,
		FirstID: 1, LastID: 12000, Rollup: strings.Repeat("ab", 32),
		Types:    map[string]int64{"message": 9000, "call": 3000},
		Warnings: 12, WarningsSuppressed: 5, Rejected: 7,
	}
}

func sampleErrorConclusion() IngestConclusion {
	c := sampleConclusion()
	c.Outcome, c.Error = "incomplete", "parser failed: bad page"
	return c
}

type auditSample struct {
	name    string
	value   any
	details map[string]any
	decode  func(map[string]any) (any, error)
	keys    []string // the detail keys the format reference promises
}

func auditSamples() []auditSample {
	start := IngestStart{
		IngestID: "ing-1", Parser: "sms", ParserVersion: "1.2.0", ParserHash: "abc123",
		AnalysisID: "an-1", Artifacts: []string{"a-1", "a-2"}, BatchRows: 5000, Reingest: true,
		NormVersion: "fts3/unicode-15.0.0/sqlite-3.45.0",
	}
	batch := BatchCommit{
		IngestID: "ing-1", BatchNo: 2, FirstID: 5001, Count: 5000, Digest: strings.Repeat("cd", 32), Created: "2026-10-04T10:00:00.123456789Z",
		Artifacts:          map[string]string{"a-1": strings.Repeat("11", 32), "a-2": strings.Repeat("22", 32)},
		ArtifactIncomplete: []string{"a-2"}, Types: map[string]int64{"message": 5000},
	}
	fail := BatchFailure{IngestID: "ing-1", BatchNo: 2, Error: "disk full"}
	end, endErr := sampleConclusion(), sampleErrorConclusion()
	recov := IngestRecover{
		IngestConclusion: IngestConclusion{
			IngestID: "ing-0", Outcome: "interrupted", Batches: 1, Records: 5000, FirstID: 1, LastID: 5000,
			Rollup: strings.Repeat("ef", 32), Types: map[string]int64{"message": 5000},
			Warnings: 2, WarningsSuppressed: 1, Rejected: 3,
		},
		ByIngestID: "ing-1", Reason: "unfinished ingest found by a later start", BatchNos: []int{2, 3}, RunMissing: true,
	}
	return []auditSample{
		{
			"IngestStart", start, start.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestStart](d) },
			[]string{"analysis_id", "artifacts", "batch_rows", "ingest_id", "norm_version", "parser", "parser_hash", "parser_version", "reingest"},
		},
		{
			"BatchCommit", batch, batch.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[BatchCommit](d) },
			[]string{"artifact_incomplete", "artifacts", "batch_no", "count", "created", "digest", "first_id", "ingest_id", "types"},
		},
		{
			"BatchFailure", fail, fail.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[BatchFailure](d) },
			[]string{"batch_no", "error", "ingest_id"},
		},
		{
			"IngestConclusion end", end, end.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestConclusion](d) },
			[]string{"batches", "first_id", "ingest_id", "last_id", "outcome", "records", "rejected", "rollup", "types", "warnings", "warnings_suppressed"},
		},
		{
			"IngestConclusion error", endErr, endErr.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestConclusion](d) },
			[]string{"batches", "error", "first_id", "ingest_id", "last_id", "outcome", "records", "rejected", "rollup", "types", "warnings", "warnings_suppressed"},
		},
		{
			"IngestRecover", recov, recov.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestRecover](d) },
			[]string{"batch_nos", "batches", "by_ingest_id", "first_id", "ingest_id", "last_id", "outcome", "reason", "records", "rejected", "rollup", "run_missing", "types", "warnings", "warnings_suppressed"},
		},
	}
}

func TestRecordAuditDetailsRoundTrip(t *testing.T) {
	c := newTestCase(t)
	for _, s := range auditSamples() {
		t.Run(s.name, func(t *testing.T) {
			keys := make([]string, 0, len(s.details))
			for k := range s.details {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, s.keys) {
				t.Errorf("detail keys = %v, want %v", keys, s.keys)
			}
			got, err := s.decode(s.details)
			if err != nil || !reflect.DeepEqual(got, s.value) {
				t.Fatalf("direct round trip: %+v, %v; want %+v", got, err, s.value)
			}

			// through the audit log, where every number comes back as json.Number
			e, err := c.Audit.Append("test.action", "", s.details)
			if err != nil {
				t.Fatal(err)
			}
			es := auditEntries(t, c)
			last := es[len(es)-1]
			if last.Seq != e.Seq {
				t.Fatalf("last entry seq = %d, want %d", last.Seq, e.Seq)
			}
			got, err = s.decode(last.Details)
			if err != nil || !reflect.DeepEqual(got, s.value) {
				t.Fatalf("audit round trip: %+v, %v; want %+v", got, err, s.value)
			}
		})
	}
}

// TestDecodeDetailsAbsentCountsAreZero: conclusion entries written before the
// warnings, warnings_suppressed and rejected keys existed still decode.
func TestDecodeDetailsAbsentCountsAreZero(t *testing.T) {
	d := sampleConclusion().Details()
	for _, k := range []string{"warnings", "warnings_suppressed", "rejected"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("Details has no %q", k)
		}
		delete(d, k)
	}
	got, err := DecodeDetails[IngestConclusion](d)
	if err != nil || got.Warnings != 0 || got.WarningsSuppressed != 0 || got.Rejected != 0 || got.Records != 12000 {
		t.Fatalf("decoded %+v, %v", got, err)
	}
}

func TestSameConclusionComparesCounts(t *testing.T) {
	a := sampleConclusion()
	if !sameConclusion(a, sampleConclusion()) {
		t.Fatal("equal conclusions differ")
	}
	for name, mod := range map[string]func(c *IngestConclusion){
		"warnings":            func(c *IngestConclusion) { c.Warnings++ },
		"warnings_suppressed": func(c *IngestConclusion) { c.WarningsSuppressed++ },
		"rejected":            func(c *IngestConclusion) { c.Rejected++ },
	} {
		b := sampleConclusion()
		mod(&b)
		if sameConclusion(a, b) {
			t.Errorf("%s is not compared", name)
		}
	}
}

func TestAuditDetailsLargeNumbersAreExact(t *testing.T) {
	c := newTestCase(t)
	x := BatchCommit{IngestID: "i", BatchNo: 1, FirstID: 9007199254740993, Count: 1, Digest: "d"} // 2^53+1
	if _, err := c.Audit.Append("test.action", "", x.Details()); err != nil {
		t.Fatal(err)
	}
	es := auditEntries(t, c)
	got, err := DecodeDetails[BatchCommit](es[len(es)-1].Details)
	if err != nil || got.FirstID != x.FirstID {
		t.Fatalf("first_id = %d, %v; want %d", got.FirstID, err, x.FirstID)
	}
}

func TestDecodeDetailsRejectsMalformed(t *testing.T) {
	good := BatchFailure{IngestID: "i", BatchNo: 1, Error: "e"}.Details()
	if _, err := DecodeDetails[BatchFailure](good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]map[string]any{
		"wrong type":    {"ingest_id": "i", "batch_no": "one", "error": "e"},
		"unknown field": {"ingest_id": "i", "batch_no": 1, "error": "e", "extra": true},
		"fractional":    {"ingest_id": "i", "batch_no": 1.5, "error": "e"},
	}
	for name, d := range bad {
		if _, err := DecodeDetails[BatchFailure](d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRecordAuditActionNames(t *testing.T) {
	for got, want := range map[string]string{
		ActionCaseUpgrade:      "case.upgrade",
		ActionCaseUpgradeDone:  "case.upgrade.done",
		ActionCaseUpgradeError: "case.upgrade.error",
		ActionIngestStart:      "records.ingest.start",
		ActionBatch:            "records.batch",
		ActionBatchError:       "records.batch.error",
		ActionIngestEnd:        "records.ingest.end",
		ActionIngestError:      "records.ingest.error",
		ActionIngestRecover:    "records.ingest.recover",
		ActionAnalysisWarning:  "analysis.warning",
	} {
		if got != want {
			t.Errorf("action constant %q, want %q", got, want)
		}
	}
}

// TestIngestWarningTallyReadsSuppressionNotes: the numbers of a suppression note are
// read as whole non-negative numbers (as the log decodes them, json.Number); anything
// else adds nothing and is counted as a bad note.
func TestIngestWarningTallyReadsSuppressionNotes(t *testing.T) {
	note := func(w, r any) map[string]any {
		return map[string]any{WarnKeySuppression: true, WarnKeySuppressedWarnings: w, WarnKeySuppressedRejects: r}
	}
	var tally IngestWarningTally
	tally.add(map[string]any{WarnKeyIngest: "x"})
	tally.add(map[string]any{WarnKeyIngest: "x", WarnKeyRejected: true})
	tally.add(note(json.Number("3"), json.Number("4")))
	if tally.Warnings != 2 || tally.Rejects != 1 || tally.Notes != 1 || tally.NoteWarnings != 3 || tally.NoteRejects != 4 ||
		tally.Suppressed() != 7 || tally.ProvenRejected() != 5 {
		t.Fatalf("tally = %+v", tally)
	}
	for name, n := range map[string]map[string]any{
		"missing":    {WarnKeySuppression: true},
		"negative":   note(json.Number("-1"), json.Number("2")),
		"fractional": note(json.Number("1.5"), json.Number("2")),
		"text":       note("3", json.Number("2")),
		"too large":  note(json.Number("99999999999999"), json.Number("2")),
		"bool":       note(true, json.Number("2")),
	} {
		var b IngestWarningTally
		b.add(n)
		if b.NoteBadCounts != 1 || b.Suppressed() != 0 || b.Warnings != 0 {
			t.Errorf("%s: tally = %+v, want one bad note adding nothing", name, b)
		}
	}
	var empty IngestWarningTally
	empty.add(note(json.Number("0"), json.Number("0")))
	if empty.NoteEmpty != 1 || empty.NoteBadCounts != 0 {
		t.Errorf("zero note: tally = %+v", empty)
	}
	var native IngestWarningTally
	native.add(note(2, float64(5)))
	if native.NoteWarnings != 2 || native.NoteRejects != 5 {
		t.Errorf("native numbers: tally = %+v", native)
	}
}

// TestIngestWarningTallyReadsBeganNote: the "suppression began" note is a marker
// only: it is counted apart (Began), never as a warning, a rejection or a
// conclusion note, and it adds no suppressed numbers.
func TestIngestWarningTallyReadsBeganNote(t *testing.T) {
	var tally IngestWarningTally
	tally.add(map[string]any{WarnKeyIngest: "x"})
	tally.add(map[string]any{WarnKeyIngest: "x", WarnKeySuppressionBegan: true})
	if tally.Warnings != 1 || tally.Began != 1 || tally.Notes != 0 || tally.Rejects != 0 || tally.Suppressed() != 0 || tally.NoteBadCounts != 0 {
		t.Fatalf("tally = %+v, want one warning and one began note", tally)
	}
	tally.add(map[string]any{WarnKeyIngest: "x", WarnKeySuppressionBegan: true})
	if tally.Began != 2 {
		t.Errorf("Began = %d after two began notes, want 2", tally.Began)
	}
	var other IngestWarningTally
	other.add(map[string]any{WarnKeyIngest: "x", WarnKeySuppressionBegan: "yes"}) // not a bool: an ordinary entry
	if other.Warnings != 1 || other.Began != 0 {
		t.Errorf("a non-bool marker was believed: %+v", other)
	}
}
