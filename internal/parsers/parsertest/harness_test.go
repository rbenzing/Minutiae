package parsertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// recorder is a testing.TB double: it collects failures instead of failing the
// real test, so a test can assert that the harness fails.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Error(args ...any) { r.errs = append(r.errs, fmt.Sprint(args...)) }
func (r *recorder) Fatalf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatal(args ...any) { r.errs = append(r.errs, fmt.Sprint(args...)) }
func (r *recorder) Failed() bool      { return len(r.errs) > 0 }

// bundle makes a harness whose failures go to tb, with one artifact name.dat.
func bundle(t *testing.T, tb testing.TB, name, data string) (*Harness, parse.Artifact, evidence.ManifestRecord) {
	t.Helper()
	h := NewHarness(tb)
	m := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/"+name+".dat", []byte(data))
	return h, h.Artifact(m, []byte(data)), m
}

func TestHarnessRunsWellBehavedParser(t *testing.T) {
	h, art, m := bundle(t, t, "well", "alpha\nbeta\ngamma\n")
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	res := h.Run(p, h.Input(p, art, nil, false))
	if len(res.Records) != 3 || len(res.Warnings) != 0 || res.Err != nil {
		t.Fatalf("records %d, warnings %v, err %v", len(res.Records), res.Warnings, res.Err)
	}
	lines := []string{"alpha", "beta", "gamma"}
	off := int64(0)
	for i, r := range res.Records {
		want := records.Range{Offset: off, Length: int64(len(lines[i]))}
		if r.Type != "message" || r.ArtifactID != m.ID || r.Body != lines[i] ||
			r.Locator != fmt.Sprintf("text:line=%d", i+1) || r.Range == nil || *r.Range != want {
			t.Errorf("record %d: %+v (range %v), want range %+v", i, r, r.Range, want)
		}
		off += int64(len(lines[i])) + 1
	}
	if res.Notes["lines"] != "3" {
		t.Errorf("notes %v", res.Notes)
	}
	if n := len(res.Progress); n == 0 || res.Progress[n-1] != [2]int64{3, 3} {
		t.Errorf("progress %v", res.Progress)
	}
	// the run went through the case writer: three records are stored
	rd, err := records.NewReader(h.Case)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, err := rd.Count(context.Background(), records.Filter{}, 0); err != nil || n != 3 {
		t.Errorf("stored records %d, err %v", n, err)
	}
}

func TestHarnessWarningsFailStrictAndAreCollectedByLenient(t *testing.T) {
	data := "a\n\nb\n"
	rec := &recorder{TB: t}
	hs, arts, _ := bundle(t, rec, "well", data)
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	hs.Run(p, hs.Input(p, arts, nil, false))
	if !rec.Failed() {
		t.Fatal("a strict run did not fail on a warning")
	}
	if !strings.Contains(strings.Join(rec.errs, "\n"), "text:line=2") {
		t.Errorf("failure does not name the locator: %v", rec.errs)
	}

	h, art, _ := bundle(t, t, "well", data)
	res := h.RunLenient(p, h.Input(p, art, nil, false))
	if len(res.Records) != 2 || len(res.Warnings) != 1 || res.Warnings[0] != (Warning{Locator: "text:line=2", Reason: "blank line"}) {
		t.Errorf("lenient: %d records, warnings %v", len(res.Records), res.Warnings)
	}
}

func TestFixtureParsesProduceNoRejections(t *testing.T) {
	rec := &recorder{TB: t}
	h, art, _ := bundle(t, rec, "well", "a\nb\n")
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	if res := h.Run(p, h.Input(p, art, nil, false)); rec.Failed() || len(res.Records) != 2 {
		t.Fatalf("well-behaved strict run: %d records, failures %v", len(res.Records), rec.errs)
	}

	for _, tc := range []struct {
		name string
		mode InvalidMode
		want error
	}{
		{"nul in summary", InvalidNulInSummary, records.ErrInvalidText},
		{"missing required field", InvalidMissingRequiredField, records.ErrInvalidPayload},
		{"unknown type (refused by the contract check before the writer)", InvalidUnknownType, parse.ErrUndeclaredType},
		{"oversize body", InvalidOversizeBody, records.ErrRecordTooLarge},
		{"bad locator", InvalidBadLocator, records.ErrInvalidLocator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := Invalid{Name: "inv", Version: "1.0.0", Mode: tc.mode, N: 3}
			rec := &recorder{TB: t}
			h, art, _ := bundle(t, rec, "inv", "x\n")
			h.Run(ip, h.Input(ip, art, nil, false))
			if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], tc.want.Error()) {
				t.Errorf("strict: want exactly one failure naming %q, got %v", tc.want, rec.errs)
			}

			h, art, _ = bundle(t, t, "inv", "x\n")
			res := h.RunLenient(ip, h.Input(ip, art, nil, false))
			if len(res.Records) != 0 || len(res.Warnings) != 3 {
				t.Fatalf("lenient: %d records, warnings %v", len(res.Records), res.Warnings)
			}
			for _, w := range res.Warnings {
				if !strings.Contains(w.Reason, tc.want.Error()) {
					t.Errorf("rejection reason %q does not name %q", w.Reason, tc.want)
				}
			}
		})
	}
}

func TestStrictEmitterUsesTheRealWriter(t *testing.T) {
	// a range beyond the artifact is a rule of records.Writer, not of this package
	rec := &recorder{TB: t}
	h, art, _ := bundle(t, rec, "liar", "tiny\n")
	p := Liar{Name: "liar", Version: "1.0.0", Mode: LieRangeBeyondArtifact}
	res := h.Run(p, h.Input(p, art, nil, false))
	if len(rec.errs) == 0 || !strings.Contains(rec.errs[0], records.ErrInvalidRange.Error()) || len(res.Records) != 0 {
		t.Fatalf("failures %v, records %d", rec.errs, len(res.Records))
	}

	// the same writer refuses an artifact the bundle did not declare
	h, art, _ = bundle(t, t, "liar", "tiny\n")
	other := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/other.dat", []byte("o\n"))
	p = Liar{Name: "liar", Version: "1.0.0", Mode: LieOutsideBundle, Other: other.ID}
	res = h.RunLenient(p, h.Input(p, art, nil, false))
	if len(res.Records) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, records.ErrUndeclaredArtifact.Error()) {
		t.Errorf("outside the bundle: %d records, %v", len(res.Records), res.Warnings)
	}
}

// retainer keeps the readers it was given.
type retainer struct {
	WellBehaved
	got io.ReaderAt
	lk  parse.Lookuper
}

func (r *retainer) Parse(_ context.Context, in *parse.Input, _ parse.Emitter) error {
	r.got, r.lk = in.Primary.R, in.Lookup
	return nil
}

func TestHarnessInputBehavesLikeTheHost(t *testing.T) {
	lim := parse.DefaultLimits()
	big := strings.Repeat("x", int(lim.ProbeBytes)+4096)
	h, art, m := bundle(t, t, "well", big)
	p := WellBehaved{Name: "well", Version: "1.0.0"}

	// forProbe: a read past ProbeBytes fails with ErrProbeLimit
	in := h.Input(p, art, nil, true)
	buf := make([]byte, len(big))
	n, err := in.Primary.R.ReadAt(buf, 0)
	if !errors.Is(err, parse.ErrProbeLimit) || int64(n) != lim.ProbeBytes {
		t.Errorf("probe read: n=%d err=%v", n, err)
	}
	// not for probe: unlimited
	in = h.Input(p, art, nil, false)
	if n, err := in.Primary.R.ReadAt(buf, 0); err != nil || n != len(big) {
		t.Errorf("parse read: n=%d err=%v", n, err)
	}
	if in.Limits != lim || in.Budget == nil || in.Budget.Limit() != lim.MemBudget || in.Lookup == nil {
		t.Errorf("limits, budget or lookuper missing: %+v", in)
	}
	role := p.Meta().Inputs[0].Role
	if in.Artifacts[role].ID != m.ID || in.Primary.ID != m.ID {
		t.Errorf("artifacts by role: %+v", in.Artifacts)
	}

	// a retained reader (and Lookuper) fails with ErrSealed after Run
	rp := &retainer{WellBehaved: p}
	in = h.Input(rp, art, nil, false)
	h.RunLenient(rp, in)
	if _, err := rp.got.ReadAt(buf[:8], 0); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("retained reader after Run: %v", err)
	}
	found := rp.lk.Find("android:**/parsertest/*.dat")
	if len(found) != 1 || found[0].ID != m.ID || found[0].R != nil {
		t.Fatalf("Find: %+v", found)
	}
	if r, err := rp.lk.Open(found[0]); err == nil {
		if _, err := r.ReadAt(buf[:8], 0); !errors.Is(err, parse.ErrSealed) {
			t.Errorf("reader opened through the Lookuper after Run: %v", err)
		}
	}
}

func TestHarnessInputIsADeepCopy(t *testing.T) {
	h, art, _ := bundle(t, t, "well", "a\n")
	art.Recovery = &parse.RecoveryInfo{Class: "carved", Method: "m"}
	art.Source.Snapshot = &parse.SnapshotInfo{Name: "snap", Xid: 7}
	p := WellBehaved{Name: "well", Version: "1.0.0"}

	in := h.Input(p, art, nil, false)
	in.Primary.Recovery.Class = "mutated"
	in.Primary.Source.Snapshot.Xid = 99
	in.Primary.SHA256 = "mutated"
	role := p.Meta().Inputs[0].Role
	a := in.Artifacts[role]
	a.Recovery = nil
	in.Artifacts[role] = a

	if art.Recovery.Class != "carved" || art.Source.Snapshot.Xid != 7 || art.SHA256 == "mutated" {
		t.Errorf("the harness artifact changed: %+v", art)
	}
	in2 := h.Input(p, art, nil, false)
	if in2.Primary.Recovery == nil || in2.Primary.Recovery.Class != "carved" || in2.Artifacts[role].Recovery == nil {
		t.Errorf("a second Input saw the first one's changes: %+v", in2.Primary)
	}
}

func TestHarnessLookuperOpensOtherArtifacts(t *testing.T) {
	h, art, _ := bundle(t, t, "well", "a\n")
	m2 := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/well.dat-wal", []byte("wal-bytes"))
	wal := h.Artifact(m2, []byte("wal-bytes"))
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	in := h.Input(p, art, map[string]parse.Artifact{"wal": wal}, false)
	found := in.Lookup.Find("android:**/parsertest/*")
	if len(found) != 2 || found[0].ID > found[1].ID {
		t.Fatalf("Find must return both, sorted by id: %+v", found)
	}
	opened := false
	for _, f := range found {
		if f.ID != m2.ID {
			continue
		}
		r, err := in.Lookup.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 9)
		if _, err := r.ReadAt(b, 0); err != nil || string(b) != "wal-bytes" {
			t.Errorf("Lookup.Open read %q, %v", b, err)
		}
		opened = true
	}
	if !opened {
		t.Error("the companion was not found")
	}
	if _, err := in.Lookup.Open(parse.Artifact{ID: "nonesuch"}); err == nil {
		t.Error("Open of an artifact that was not found succeeded")
	}
}

func TestHarnessSurvivesAParserPanic(t *testing.T) {
	h, art, _ := bundle(t, t, "pan", "a\n")
	pp := Panicking{Name: "pan", Version: "1.0.0", Where: PanicInParse, Value: "boom"}
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v", r)
			}
		}()
		h.RunLenient(pp, h.Input(pp, art, nil, false))
	}()
	// the live-ingest slot was released: a normal run on the same case still works
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	m := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/well.dat", []byte("a\n"))
	if res := h.Run(p, h.Input(p, h.Artifact(m, []byte("a\n")), nil, false)); len(res.Records) != 1 {
		t.Errorf("after a panic: %d records", len(res.Records))
	}
}
