package artparse_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// bundleParser emits one message record on every artifact of its bundle and keeps what the host told it
// about each artifact (the recovery description) and what its Lookuper found.
type bundleParser struct {
	name string
	mu   sync.Mutex
	seen map[string]*parse.RecoveryInfo // artifact id -> Input recovery of that artifact
	find []string                       // ids a Lookuper.Find of dbGlob returned in the last Parse
}

const dbGlob = "android:/data/data/*/databases/app.db"

func (p *bundleParser) Meta() parse.Meta { return metaFor(p.name, "1.0.0") }

func (p *bundleParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: parse.Applicable}, nil
}

func (p *bundleParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	roles := make([]string, 0, len(in.Artifacts))
	for r := range in.Artifacts {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var found []string
	for _, a := range in.Lookup.Find(dbGlob) {
		found = append(found, a.ID)
	}
	p.mu.Lock()
	if p.seen == nil {
		p.seen = map[string]*parse.RecoveryInfo{}
	}
	p.find = found
	for _, r := range roles {
		a := in.Artifacts[r]
		p.seen[a.ID] = a.Recovery
	}
	p.mu.Unlock()
	for i, r := range roles {
		a := in.Artifacts[r]
		rec := records.Record{
			Type: message.Type, ArtifactID: a.ID, Locator: "text:line=" + string(rune('1'+i)),
			Summary: "on " + r, Payload: recordstest.ValidPayload(message.Type),
		}
		if err := out.Emit(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

// recoveredFixture is a case with an Android acquisition and a parser registered over it.
type recoveredFixture struct {
	*rx
	p *bundleParser
}

func newRecoveredFixture(t *testing.T) *recoveredFixture {
	t.Helper()
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	return &recoveredFixture{rx: &rx{t: t, c: c, recs: map[string]evidence.ManifestRecord{}}, p: &bundleParser{name: "bundle"}}
}

// build makes the host over the fixture's case; each mod changes the built host (test seams).
func (f *recoveredFixture) build(mod ...func(*artparse.Host)) *artparse.Host {
	h := f.host(nil, f.p)
	for _, m := range mod {
		m(h)
	}
	return h
}

// recoverArtifact captures a recovered file at fsPath whose Recovery holds the given confidence.
func (f *recoveredFixture) recoverArtifact(rel, fsPath, parent, parentSHA string, conf *int) evidence.ManifestRecord {
	f.t.Helper()
	d := &evidence.Derivation{
		ParentID: parent, ParentSHA256: parentSHA, FSType: "ext4", FSPath: fsPath,
		Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: conf},
	}
	return put(f.t, f.c, "D1", "A1", rel, evidence.Source{Kind: "recover", RemotePath: fsPath, Derived: d}, "x")
}

// extractOf captures a live extract (no Recovery of its own) derived from the artifact with id parent.
func (f *recoveredFixture) extractOf(rel, fsPath, parent, parentSHA string) evidence.ManifestRecord {
	f.t.Helper()
	d := &evidence.Derivation{ParentID: parent, ParentSHA256: parentSHA, FSType: "ext4", FSPath: fsPath}
	return put(f.t, f.c, "D1", "A1", rel, evidence.Source{Kind: "extract", RemotePath: fsPath, Derived: d}, "x")
}

func TestRecoveredInputCarriesTrail(t *testing.T) {
	c55 := 55
	for _, tc := range []struct {
		name         string
		parentExists bool
		conf         *int
		want         parse.RecoveryInfo
	}{
		{"complete trail", true, &c55, parse.RecoveryInfo{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &c55}},
		{"cut trail: the parent is missing", false, &c55, parse.RecoveryInfo{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &c55, CapUnknown: true}},
		{"no confidence on the chain", true, nil, parse.RecoveryInfo{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", CapUnknown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveredFixture(t)
			parent, parentSHA := "missing", ""
			if tc.parentExists {
				pr := putFile(t, f.c, "D1", "A1", "/data/parent.bin", "p")
				parent, parentSHA = pr.ID, pr.SHA256
			}
			rec := f.recoverArtifact("rec", dbPath, parent, parentSHA, tc.conf)
			ext := f.extractOf("ext", dbPath2, rec.ID, rec.SHA256)
			sum := f.run(f.build(), artparse.Selection{IncludeRecovered: true})
			if len(sum.Jobs) != 2 {
				t.Fatalf("%d jobs, want the recovered artifact and its extract: %+v", len(sum.Jobs), sum.Jobs)
			}
			for _, j := range sum.Jobs {
				if j.Outcome != artparse.OutcomeComplete {
					t.Errorf("job %+v", j)
				}
			}
			f.p.mu.Lock()
			defer f.p.mu.Unlock()
			for name, id := range map[string]string{"recovered artifact": rec.ID, "extract of it": ext.ID} {
				got := f.p.seen[id]
				if got == nil || !reflect.DeepEqual(*got, tc.want) {
					t.Errorf("%s: Input recovery = %+v, want %+v", name, got, tc.want)
				}
			}
		})
	}
}

func TestPlanMarksRecoveredJobs(t *testing.T) {
	c55 := 55
	f := newRecoveredFixture(t)
	live := putFile(t, f.c, "D1", "A1", dbPath, "1")
	rec := f.recoverArtifact("rec", dbPath2, "missing", "", &c55)
	h := f.build()

	p := planOf(t, h, artparse.Selection{})
	if len(p.Rows) != 1 || p.Rows[0].Job.Primary.Artifact.ID != live.ID || p.Rows[0].Recovered != nil {
		t.Fatalf("without the flag: rows %+v, want only the live job, unmarked", p.Rows)
	}
	if p.Counts.RecoveredExcluded != 1 {
		t.Errorf("RecoveredExcluded = %d, want 1", p.Counts.RecoveredExcluded)
	}

	p = planOf(t, h, artparse.Selection{IncludeRecovered: true})
	if len(p.Rows) != 2 || p.Counts.RecoveredExcluded != 0 {
		t.Fatalf("with the flag: %d rows, counts %+v", len(p.Rows), p.Counts)
	}
	for _, r := range p.Rows {
		switch r.Job.Primary.Artifact.ID {
		case live.ID:
			if r.Recovered != nil {
				t.Errorf("the live job is marked recovered: %+v", r.Recovered)
			}
		case rec.ID:
			want := parse.RecoveryInfo{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &c55, CapUnknown: true} // the parent is missing
			if r.Recovered == nil || !reflect.DeepEqual(*r.Recovered, want) {
				t.Errorf("recovered job: Recovered = %+v, want %+v", r.Recovered, want)
			}
		default:
			t.Errorf("unexpected row %+v", r)
		}
	}
}

func payloadOf(t *testing.T, c *evidence.Case, id int64) map[string]any {
	t.Helper()
	rd, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	full, err := rd.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(full.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A recovered database read together with a live -wal: the -wal may describe a state the recovered
// database never had, so no record of the job is live. The record on the recovered artifact says it is
// that artifact; the record on the live member says it comes from the bundle.
func TestMixedBundleRecordsAreAllRecovered(t *testing.T) {
	c55 := 55
	f := newRecoveredFixture(t)
	db := f.recoverArtifact("db", dbPath, "missing", "", &c55)
	wal := f.extractOf("wal", walPath, "missing", "")
	sum := f.run(f.build(), artparse.Selection{IncludeRecovered: true})
	if j := jobOf(t, sum, "bundle"); j.Outcome != artparse.OutcomeComplete || j.Rejected != 0 {
		t.Fatalf("job %+v", j)
	}
	rows := f.rows(false)
	if len(rows) != 2 {
		t.Fatalf("%d records, want one per artifact", len(rows))
	}
	for _, r := range rows {
		if !r.Deleted || !r.Recovered || r.Method != "fat-contiguous" || r.Confidence == nil || *r.Confidence > 55 {
			t.Errorf("record %+v, want deleted, recovered by fat-contiguous, confidence <= 55", r)
		}
		rec, _ := payloadOf(t, f.c, r.ID)["recovery"].(map[string]any)
		switch r.ArtifactID {
		case db.ID:
			if rec["relation"] != "from-recovered-artifact" {
				t.Errorf("record on the recovered database: payload.recovery = %v", rec)
			}
			if _, ok := rec["bundle_member"]; ok {
				t.Errorf("record on the recovered database names a bundle member: %v", rec)
			}
		case wal.ID:
			notes, _ := rec["notes"].([]any)
			if rec["relation"] != "from-recovered-bundle" || rec["bundle_member"] != db.ID || rec["artifact_method"] != "fat-contiguous" || !slices.Contains(notes, any("mixed-bundle")) {
				t.Errorf("record on the live wal: payload.recovery = %v, want relation from-recovered-bundle, bundle_member %s, the note mixed-bundle", rec, db.ID)
			}
		default:
			t.Errorf("record on unexpected artifact %s", r.ArtifactID)
		}
	}
	if end := f.jobEnds()[0]; end.Notes["mixed_bundle"] != db.ID {
		t.Errorf("job notes %v, want mixed_bundle = %s", end.Notes, db.ID)
	}
}

// With the host's propagation off (a test seam), the records writer itself refuses a live record on a
// recovered artifact: the second line of defence is real and counted.
func TestRecoveredArtifactLiveRecordRejectedByWriter(t *testing.T) {
	c55 := 55
	f := newRecoveredFixture(t)
	f.recoverArtifact("db", dbPath, "missing", "", &c55)
	sum := f.run(f.build(artparse.DisablePropagation), artparse.Selection{IncludeRecovered: true})
	j := jobOf(t, sum, "bundle")
	if j.Outcome != artparse.OutcomeIncomplete || j.Rejected != 1 {
		t.Fatalf("job %+v, want incomplete with 1 rejected", j)
	}
	if end := f.jobEnds()[0]; end.Rejected != 1 || end.Reason == "" {
		t.Errorf("job.end %+v", end)
	}
	if n := len(f.rows(true)); n != 0 {
		t.Errorf("%d records were stored, want none", n)
	}
	// and the same run with propagation on stores a recovered record
	f2 := newRecoveredFixture(t)
	f2.recoverArtifact("db", dbPath, "missing", "", &c55)
	f2.run(f2.build(), artparse.Selection{IncludeRecovered: true})
	if rows := f2.rows(false); len(rows) != 1 || !rows[0].Deleted || !rows[0].Recovered {
		t.Errorf("propagation on: %+v", rows)
	}
}

// A live job's Lookuper does not hand out an artifact that is recovered input.
func TestLookupExcludesArtifactsWithARecoveredAncestor(t *testing.T) {
	f := newRecoveredFixture(t)
	c55 := 55
	live := putFile(t, f.c, "D1", "A1", dbPath, "1")
	rec := f.recoverArtifact("rec", dbPath2, "missing", "", &c55)
	f.extractOf("ext", dbPath3, rec.ID, rec.SHA256)
	liveExt := putExtract(t, f.c, "D1", "A1", "liveext", "ext4", "/data/data/com.d/databases/app.db", nil)
	f.run(f.build(), artparse.Selection{})
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	got := slices.Clone(f.p.find)
	slices.Sort(got)
	want := []string{live.ID, liveExt.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Find returned %v, want only the live artifacts %v", got, want)
	}
}
