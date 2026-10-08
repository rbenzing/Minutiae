package artparse_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

const libGlob = "ios:HomeDomain/Library/*.db"

func iosParser(t testing.TB) artparse.Registered {
	t.Helper()
	m := parse.Meta{
		Name: "ios-p", Version: "1.0.0", Title: "ios test parser",
		Platforms: []string{parse.PlatformIOS},
		Emits:     metaFor("x", "1.0.0").Emits,
		Inputs:    []parse.InputSpec{{Role: "db", Globs: []string{libGlob}, Required: true}},
	}
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("ios-p"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ids(as []parse.Artifact) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func TestLookupFindSortedAndLiveOnly(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	const lib = "/private/var/mobile/Library/"
	live1 := putExtract(t, c, "D1", "A1", "a/live1", "apfs", lib+"c.db", nil)
	live2 := putExtract(t, c, "D1", "A1", "a/live2", "apfs", lib+"a.db", nil)
	s1 := putExtract(t, c, "D1", "A1", "a/s1", "apfs", lib+"s1.db", &evidence.SnapshotRef{Name: "one", Xid: 1})
	s2 := putExtract(t, c, "D1", "A1", "a/s2", "apfs", lib+"s2.db", &evidence.SnapshotRef{Name: "two", Xid: 2})
	rec := put(t, c, "D1", "A1", "a/carved", evidence.Source{Kind: "carved", RemotePath: lib + "carved.db"}, "x")
	h := newHost(t, c, iosParser(t))
	snap := snapshotOf(t, h)

	open := func(job artparse.Job) *artparse.Bundle {
		b, err := artparse.OpenBundle(context.Background(), h, snap, job, "p1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("live jobs %v err %v", jobs, err)
	}
	found := open(jobs[0]).Input(false).Lookup.Find(libGlob)
	want := []string{live1.ID, live2.ID}
	slices.Sort(want)
	if !slices.Equal(ids(found), want) {
		t.Errorf("live job: found %v, want %v (no snapshot artifact, no recovered one %s)", ids(found), want, rec.ID)
	}
	for _, a := range found {
		if a.R != nil || a.Size == 0 || a.SHA256 == "" || a.Logical == "" || a.Platform != parse.PlatformIOS {
			t.Errorf("found artifact %+v: R must be nil and the facts set", a)
		}
	}
	if got := open(jobs[0]).Input(false).Lookup.Find("android:/**"); len(got) != 0 {
		t.Errorf("a glob of another platform found %v", ids(got))
	}

	sjobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{IncludeSnapshots: true})
	if err != nil {
		t.Fatal(err)
	}
	var job1 *artparse.Job
	for i, j := range sjobs {
		if j.Primary.Artifact.ID == s1.ID {
			job1 = &sjobs[i]
		}
	}
	if job1 == nil {
		t.Fatalf("no job over the snapshot artifact: %v", jobIDs(sjobs))
	}
	found = open(*job1).Input(false).Lookup.Find(libGlob)
	want = []string{live1.ID, live2.ID, s1.ID}
	slices.Sort(want)
	if !slices.Equal(ids(found), want) {
		t.Errorf("snapshot job: found %v, want %v (its own snapshot only, not %s)", ids(found), want, s2.ID)
	}
}

func lookupFixture(t *testing.T, limit int) (*fixture, []evidence.ManifestRecord) {
	t.Helper()
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	f := &fixture{c: c}
	f.db = putFile(t, c, "D1", "A1", dbPath, "hello")
	var others []evidence.ManifestRecord
	for _, n := range []string{"o1", "o2", "o3"} {
		others = append(others, putFile(t, c, "D1", "A1", "/data/data/com.a/databases/"+n+".dat", "data-"+n))
	}
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.MaxLookupOpens = limit })}
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	f.h, f.snap = h, snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), f.snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	f.job = jobs[0]
	return f, others
}

const datGlob = "android:/data/data/com.a/databases/*.dat"

func TestLookupOpenVerifiesSealsAndBounds(t *testing.T) {
	t.Run("hash mismatch is refused and the limit counts every open", func(t *testing.T) {
		f, others := lookupFixture(t, 2)
		writeFile(t, f.file(others[1]), "DATA-O2")
		in := f.open(t).Input(false)
		found := byRecord(t, in.Lookup.Find(datGlob), others)
		if r, err := in.Lookup.Open(found[0]); err != nil || readAll(t, r, 7) != "data-o1" {
			t.Fatalf("first open: %v", err)
		}
		var hm *artparse.HashMismatchError
		if r, err := in.Lookup.Open(found[1]); !errors.As(err, &hm) || r != nil {
			t.Errorf("tampered artifact: reader %v, err %v", r, err)
		}
		if r, err := in.Lookup.Open(found[2]); !errors.Is(err, artparse.ErrLookupOpens) || r != nil {
			t.Errorf("the 3rd open: reader %v, err %v, want ErrLookupOpens", r, err)
		}
	})
	t.Run("recorded for the audit and sealed with the bundle", func(t *testing.T) {
		f, others := lookupFixture(t, 4)
		b := f.open(t)
		in := b.Input(false)
		found := byRecord(t, in.Lookup.Find(datGlob), others)
		r1, err := in.Lookup.Open(found[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := in.Lookup.Open(found[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := in.Lookup.Open(parse.Artifact{ID: "nonesuch"}); !errors.Is(err, evidence.ErrUnknownArtifact) {
			t.Errorf("an unknown artifact: %v", err)
		}
		if _, err := in.Lookup.Open(parse.Artifact{ID: f.db.ID}); err != nil {
			t.Errorf("the bundle's own primary can be opened too: %v", err)
		}
		want := []artparse.LookupOpen{
			{ArtifactID: others[0].ID, SHA256: others[0].SHA256},
			{ArtifactID: others[2].ID, SHA256: others[2].SHA256},
			{ArtifactID: f.db.ID, SHA256: f.db.SHA256},
		}
		if got := b.Lookups(); !slices.Equal(got, want) {
			t.Errorf("lookups %v, want %v", got, want)
		}
		b.Seal()
		if _, err := r1.ReadAt(make([]byte, 2), 0); !errors.Is(err, parse.ErrSealed) {
			t.Errorf("a Lookuper reader after the seal: %v", err)
		}
	})
	t.Run("an artifact that is not eligible is refused", func(t *testing.T) {
		f, _ := lookupFixture(t, 4)
		rec := put(t, f.c, "D1", "A1", "x/carved", evidence.Source{Kind: "carved", RemotePath: "/x"}, "x")
		f.snap = snapshotOf(t, f.h)
		in := f.open(t).Input(false)
		if _, err := in.Lookup.Open(parse.Artifact{ID: rec.ID}); err == nil {
			t.Error("a carved artifact was opened through the Lookuper")
		}
	})
	t.Run("in-memory total", func(t *testing.T) {
		c := newCase(t)
		acquireStart(t, c, "D1", "A1", "logical")
		db := putFile(t, c, "D1", "A1", dbPath, "hello")
		o1 := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/o1.dat", "data-o1")
		opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.MemInputMax = 7; l.MemInputTotal = 7 })}
		h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{c: c, h: h, snap: snapshotOf(t, h), db: db}
		jobs, _, err := h.Discover(context.Background(), f.snap, artparse.Selection{})
		if err != nil || len(jobs) != 1 {
			t.Fatal(jobs, err)
		}
		f.job = jobs[0]
		in := f.open(t).Input(false) // the primary (5 bytes) is in memory: 2 bytes of the total are left
		r, err := in.Lookup.Open(parse.Artifact{ID: o1.ID})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, f.file(o1), "DATA-O1")
		if got := readAll(t, r, 7); got != "DATA-O1" {
			t.Errorf("a lookup open past the in-memory total must stream: read %q", got)
		}
	})
}

func TestRecheckCoversLookupOpens(t *testing.T) {
	f, others := lookupFixture(t, 3)
	b := f.open(t)
	in := b.Input(false)
	found := byRecord(t, in.Lookup.Find(datGlob), others)
	r, err := in.Lookup.Open(found[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Recheck(context.Background()); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	writeFile(t, f.file(others[0]), "DATA-O1")
	if got := readAll(t, r, 7); got != "data-o1" {
		t.Errorf("the opened reader must serve the verified bytes: %q", got)
	}
	if err := b.Recheck(context.Background()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("recheck after changing a looked-up artifact: %v", err)
	}
}

// byRecord orders found like recs (o1, o2, o3), whatever the artifact ids sort to.
func byRecord(t testing.TB, found []parse.Artifact, recs []evidence.ManifestRecord) []parse.Artifact {
	t.Helper()
	if len(found) != len(recs) {
		t.Fatalf("found %v, want %d artifacts", ids(found), len(recs))
	}
	if !slices.IsSortedFunc(found, func(a, b parse.Artifact) int { return strings.Compare(a.ID, b.ID) }) {
		t.Errorf("Find is not sorted by artifact id: %v", ids(found))
	}
	var out []parse.Artifact
	for _, r := range recs {
		i := slices.IndexFunc(found, func(a parse.Artifact) bool { return a.ID == r.ID })
		if i < 0 {
			t.Fatalf("artifact %s was not found", r.ID)
		}
		out = append(out, found[i])
	}
	return out
}
