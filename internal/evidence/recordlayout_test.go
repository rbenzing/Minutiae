package evidence_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func layoutCodecs() map[string]struct {
	details map[string]any
	decode  func(map[string]any) (any, error)
	want    any
} {
	type row = struct {
		details map[string]any
		decode  func(map[string]any) (any, error)
		want    any
	}
	conc := evidence.IngestConclusion{IngestID: "i", Outcome: "complete", Types: map[string]int64{}}
	start := evidence.IngestStart{IngestID: "i", Artifacts: []string{"a"}}
	batch := evidence.BatchCommit{IngestID: "i", BatchNo: 1, FirstID: 1, Count: 1, Artifacts: map[string]string{}, ArtifactIncomplete: []string{}, Types: map[string]int64{}}
	fail := evidence.BatchFailure{IngestID: "i", BatchNo: 1, Error: "x"}
	recov := evidence.IngestRecover{IngestConclusion: conc, ByIngestID: "j", BatchNos: []int{2}}
	rs := evidence.ReindexStart{ReindexID: "r", Tables: []string{"t"}}
	rd := evidence.ReindexDone{ReindexID: "r", Docs: map[string]int64{"t": 1}}
	rf := evidence.ReindexFailure{ReindexID: "r", Error: "x"}
	return map[string]row{
		"IngestStart":      {start.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.IngestStart](d) }, start},
		"BatchCommit":      {batch.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.BatchCommit](d) }, batch},
		"BatchFailure":     {fail.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.BatchFailure](d) }, fail},
		"IngestConclusion": {conc.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.IngestConclusion](d) }, conc},
		"IngestRecover":    {recov.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.IngestRecover](d) }, recov},
		"ReindexStart":     {rs.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.ReindexStart](d) }, rs},
		"ReindexDone":      {rd.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.ReindexDone](d) }, rd},
		"ReindexFailure":   {rf.Details(), func(d map[string]any) (any, error) { return evidence.DecodeDetails[evidence.ReindexFailure](d) }, rf},
	}
}

func TestRecordsAuditDetailsCarryLayout(t *testing.T) {
	cs := layoutCodecs()
	if len(cs) != 8 {
		t.Fatalf("%d codecs, want 8", len(cs))
	}
	for name, c := range cs {
		t.Run(name, func(t *testing.T) {
			if got := c.details["layout"]; got != json.Number("1") {
				t.Fatalf("layout = %#v, want json.Number(1)", got)
			}
			got, err := c.decode(c.details)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("round trip = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestDecodeDetailsAbsentLayoutIsOne (PASSES ON ARRIVAL: absent was always read; mutation: treat absent as an error)
// pins that every entry written before layouts existed still decodes and verifies.
func TestDecodeDetailsAbsentLayoutIsOne(t *testing.T) {
	dir := recordstest.CopyV2Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Action, "records.") {
			continue
		}
		if _, has := e.Details["layout"]; has {
			t.Fatalf("seq %d of the committed v2 case carries a layout", e.Seq)
		}
		n++
		var err error
		switch e.Action {
		case evidence.ActionIngestStart:
			_, err = evidence.DecodeDetails[evidence.IngestStart](e.Details)
		case evidence.ActionBatch:
			_, err = evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		case evidence.ActionBatchError:
			_, err = evidence.DecodeDetails[evidence.BatchFailure](e.Details)
		case evidence.ActionIngestEnd, evidence.ActionIngestError:
			_, err = evidence.DecodeDetails[evidence.IngestConclusion](e.Details)
		case evidence.ActionIngestRecover:
			_, err = evidence.DecodeDetails[evidence.IngestRecover](e.Details)
		}
		if err != nil {
			t.Errorf("seq %d %s: %v", e.Seq, e.Action, err)
		}
	}
	if n == 0 {
		t.Fatal("the v2 case holds no records.* entry")
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func TestDecodeDetailsNewerLayoutIsTyped(t *testing.T) {
	d := evidence.BatchCommit{IngestID: "i", BatchNo: 1}.Details()
	d["layout"] = json.Number("2")
	d["z"] = json.Number("1")
	_, err := evidence.DecodeDetails[evidence.BatchCommit](d)
	if !errors.Is(err, evidence.ErrAuditLayoutNewer) {
		t.Fatalf("err = %v, want ErrAuditLayoutNewer", err)
	}
	var le *evidence.AuditLayoutError
	if !errors.As(err, &le) || le.Layout != 2 || le.Known != 1 {
		t.Fatalf("AuditLayoutError = %+v", le)
	}
	if strings.Contains(err.Error(), "unknown field") {
		t.Errorf("message %q names an unknown field", err)
	}
	if _, has := d["layout"]; !has {
		t.Error("DecodeDetails modified the caller's map")
	}
}

func TestDecodeDetailsBadLayout(t *testing.T) {
	for _, v := range []any{json.Number("0"), json.Number("-1"), json.Number("1.5"), "1", true, json.Number("9223372036854775808"), float64(1.5), nil} {
		d := evidence.BatchCommit{IngestID: "i", BatchNo: 1}.Details()
		d["layout"] = v
		_, err := evidence.DecodeDetails[evidence.BatchCommit](d)
		if err == nil || errors.Is(err, evidence.ErrAuditLayoutNewer) {
			t.Errorf("layout %#v: err = %v, want a plain decode error", v, err)
		}
	}
	// the integers 1 as a float64 or int (a map not read from the log) still decode
	for _, v := range []any{float64(1), 1, int64(1), json.Number("1")} {
		d := evidence.BatchCommit{IngestID: "i", BatchNo: 1}.Details()
		d["layout"] = v
		if _, err := evidence.DecodeDetails[evidence.BatchCommit](d); err != nil {
			t.Errorf("layout %#v: %v", v, err)
		}
	}
}

func TestVerifyNewerLayoutIsNamedNotUnknownField(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	d := evidence.BatchCommit{IngestID: "ing-future", BatchNo: 1, FirstID: 100, Count: 1, Artifacts: map[string]string{}, ArtifactIncomplete: []string{}, Types: map[string]int64{}}.Details()
	d["layout"] = json.Number("2")
	d["z"] = json.Number("1")
	e, err := c.Audit.Append(evidence.ActionBatch, "", d)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	want := "audit seq " + itoa(e.Seq) + ": records.batch was written with records audit layout 2; this build reads layouts up to 1 (verify it with the build that wrote it)"
	evidencetest.RequireProblem(t, rep, want)
	evidencetest.RequireNoProblem(t, rep, "unknown field")
	if rep.OK() || rep.CancelledOnly() {
		t.Errorf("OK=%v CancelledOnly=%v, want a plain integrity failure", rep.OK(), rep.CancelledOnly())
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
