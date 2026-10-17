package artparse_test

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Rulings T25 and T46: Artifact.Acquisition is the acquisition of the manifest path (artifacts/<device>/<acquisition>)
// on the members of a job and on the results of Lookup.Find, so a parser can tell two acquisitions of one device apart.
func TestArtifactsCarryTheirAcquisition(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	acquireStart(t, c, "D1", "A2", "logical")
	const other = "/data/data/com.a/databases/other.dat"
	putFile(t, c, "D1", "A1", dbPath, "hello")
	a1 := putFile(t, c, "D1", "A1", other, "one")
	a2 := putFile(t, c, "D1", "A2", other, "two")
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, artparse.Options{Limits: parse.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var job *artparse.Job
	for i, j := range jobs {
		if j.Primary.Artifact.Path == "artifacts/D1/A1/files/"+dbPath[1:] {
			job = &jobs[i]
		}
	}
	if job == nil {
		t.Fatalf("no job over the A1 database: %d jobs", len(jobs))
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, *job, "p1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	in := b.Input(false)
	if in.Primary.Acquisition != "artifacts/D1/A1" || in.Artifacts["db"].Acquisition != "artifacts/D1/A1" {
		t.Errorf("member acquisition %q / %q, want artifacts/D1/A1", in.Primary.Acquisition, in.Artifacts["db"].Acquisition)
	}
	got := map[string]string{}
	for _, a := range in.Lookup.Find("android:" + other) {
		got[a.ID] = a.Acquisition
	}
	if len(got) != 2 || got[a1.ID] != "artifacts/D1/A1" || got[a2.ID] != "artifacts/D1/A2" {
		t.Errorf("Find acquisitions %v, want %s in A1 and %s in A2", got, a1.ID, a2.ID)
	}
}
