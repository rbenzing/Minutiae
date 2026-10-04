package evidence

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func sampleConclusion() IngestConclusion {
	return IngestConclusion{
		IngestID: "ing-1", Outcome: "complete", Batches: 3, Records: 12000,
		FirstID: 1, LastID: 12000, Rollup: strings.Repeat("ab", 32),
		Types: map[string]int64{"message": 9000, "call": 3000},
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
	}
	batch := BatchCommit{
		IngestID: "ing-1", BatchNo: 2, FirstID: 5001, Count: 5000, Digest: strings.Repeat("cd", 32),
		Artifacts:          map[string]string{"a-1": strings.Repeat("11", 32), "a-2": strings.Repeat("22", 32)},
		ArtifactIncomplete: []string{"a-2"}, Types: map[string]int64{"message": 5000},
	}
	fail := BatchFailure{IngestID: "ing-1", BatchNo: 2, Error: "disk full"}
	end, endErr := sampleConclusion(), sampleErrorConclusion()
	recov := IngestRecover{
		IngestConclusion: IngestConclusion{
			IngestID: "ing-0", Outcome: "interrupted", Batches: 1, Records: 5000, FirstID: 1, LastID: 5000,
			Rollup: strings.Repeat("ef", 32), Types: map[string]int64{"message": 5000},
		},
		ByIngestID: "ing-1", Reason: "unfinished ingest found by a later start", BatchNos: []int{2, 3}, RunMissing: true,
	}
	return []auditSample{
		{
			"IngestStart", start, start.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestStart](d) },
			[]string{"analysis_id", "artifacts", "batch_rows", "ingest_id", "parser", "parser_hash", "parser_version", "reingest"},
		},
		{
			"BatchCommit", batch, batch.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[BatchCommit](d) },
			[]string{"artifact_incomplete", "artifacts", "batch_no", "count", "digest", "first_id", "ingest_id", "types"},
		},
		{
			"BatchFailure", fail, fail.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[BatchFailure](d) },
			[]string{"batch_no", "error", "ingest_id"},
		},
		{
			"IngestConclusion end", end, end.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestConclusion](d) },
			[]string{"batches", "first_id", "ingest_id", "last_id", "outcome", "records", "rollup", "types"},
		},
		{
			"IngestConclusion error", endErr, endErr.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestConclusion](d) },
			[]string{"batches", "error", "first_id", "ingest_id", "last_id", "outcome", "records", "rollup", "types"},
		},
		{
			"IngestRecover", recov, recov.Details(),
			func(d map[string]any) (any, error) { return DecodeDetails[IngestRecover](d) },
			[]string{"batch_nos", "batches", "by_ingest_id", "first_id", "ingest_id", "last_id", "outcome", "reason", "records", "rollup", "run_missing", "types"},
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
