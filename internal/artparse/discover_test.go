package artparse_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const (
	dbPath  = "/data/data/com.a/databases/app.db"
	walPath = dbPath + "-wal"
)

func TestLimitsMinimumsEnforcedInProduction(t *testing.T) {
	c := newCase(t)
	lim := parse.DefaultLimits()
	lim.Timeout = 50 * 1000 * 1000 // 50 ms
	if _, err := artparse.New(c, nil, artparse.Options{Limits: lim}); err == nil {
		t.Error("New accepted a 50 ms timeout")
	}
	lim = parse.DefaultLimits()
	lim.GracePeriod = 1 * 1000 * 1000 // 1 ms
	if _, err := artparse.New(c, nil, artparse.Options{Limits: lim}); err == nil {
		t.Error("New accepted a 1 ms grace period")
	}
	if err := (parse.Limits{}).Validate(); err == nil {
		t.Fatal("parse.Limits.Validate must stay strict")
	}
	lim = parse.DefaultLimits()
	lim.Timeout = 50 * 1000 * 1000
	lim.GracePeriod = 1 * 1000 * 1000
	opt := artparse.Options{Limits: lim}
	artparse.SkipLimitMinimums(&opt)
	if _, err := artparse.New(c, nil, opt); err != nil {
		t.Errorf("New with skipLimitMinimums: %v", err)
	}
	// the test option lowers only those minimums
	opt.Limits.MaxRecords = 0
	if _, err := artparse.New(c, nil, opt); err == nil {
		t.Error("skipLimitMinimums must not disable the other limit checks")
	}
}

func TestNewValidatesRegistry(t *testing.T) {
	c := newCase(t)
	a, b := register(t, "p", "1.0.0"), register(t, "p", "2.0.0")
	if _, err := artparse.New(c, []artparse.Registered{a, b}, artparse.Options{Limits: parse.DefaultLimits()}); err == nil {
		t.Error("two parsers named p were accepted")
	}
	if _, err := artparse.New(nil, nil, artparse.Options{Limits: parse.DefaultLimits()}); err == nil {
		t.Error("a nil case was accepted")
	}
	if _, err := artparse.New(c, []artparse.Registered{{}}, artparse.Options{Limits: parse.DefaultLimits()}); err == nil {
		t.Error("an unregistered (zero) parser was accepted")
	}
}

func TestDiscoveryOneJobPerPrimaryMatch(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db1 := putFile(t, c, "D1", "A1", dbPath, "1")
	wal1 := putFile(t, c, "D1", "A1", walPath, "w")
	db2 := putFile(t, c, "D1", "A1", "/data/user/10/com.a/databases/app.db", "2")
	putFile(t, c, "D1", "A1", "/data/data/com.a/databases/other.db", "3")

	jobs, counts := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want 2", len(jobs))
	}
	byID := map[string]artparse.Job{}
	for _, j := range jobs {
		byID[j.Primary.Artifact.ID] = j
		if j.Status != "new" || j.Explicit || j.Primary.Role != "db" || j.Primary.Platform != "android" || j.Primary.Namer != "device-file" {
			t.Errorf("job %+v", j)
		}
	}
	j1, j2 := byID[db1.ID], byID[db2.ID]
	if j1.Primary.Logical != "android:"+dbPath || j2.Primary.Logical != "android:/data/user/10/com.a/databases/app.db" {
		t.Errorf("logical paths %q %q", j1.Primary.Logical, j2.Primary.Logical)
	}
	if w, ok := j1.Others["wal"]; !ok || w.Artifact.ID != wal1.ID || w.Role != "wal" || w.Logical != "android:"+walPath {
		t.Errorf("job 1 wal member = %+v, %v", w, ok)
	}
	if len(j2.Others) != 0 {
		t.Errorf("job 2 has companions %v, but its wal is the other database's", j2.Others)
	}
	if counts.Manifest != 4 || counts.ByKind["file"] != 4 || counts.NotParserInput != 0 {
		t.Errorf("counts %+v", counts)
	}
}

func TestDiscoveryBundlesSameAcquisitionOnly(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	acquireStart(t, c, "D1", "A2", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "1")
	otherWal := putFile(t, c, "D1", "A2", walPath, "w2")
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Primary.Artifact.ID != db.ID || len(jobs[0].Others) != 0 {
		t.Fatalf("a wal of another acquisition was bundled: %+v", jobs)
	}
	sameWal := putFile(t, c, "D1", "A1", walPath, "w1")
	jobs, _ = discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Others["wal"].Artifact.ID != sameWal.ID || jobs[0].Others["wal"].Artifact.ID == otherWal.ID {
		t.Fatalf("the same-acquisition wal was not picked: %+v", jobs[0].Others)
	}
}

func TestDiscoveryAmbiguousCompanionIsUnparsed(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", dbPath, "1")
	w1 := put(t, c, "D1", "A1", "wal-one", evidence.Source{Kind: "file", RemotePath: walPath}, "a")
	w2 := put(t, c, "D1", "A1", "wal-two", evidence.Source{Kind: "file", RemotePath: walPath}, "b")
	jobs, _ := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	j := jobs[0]
	if j.Status != "unparsed" || !strings.Contains(j.Reason, "ambiguous companion") || !strings.Contains(j.Reason, w1.ID) || !strings.Contains(j.Reason, w2.ID) {
		t.Errorf("status %q reason %q", j.Status, j.Reason)
	}
	if _, ok := j.Others["wal"]; ok {
		t.Error("an ambiguous role must not be bundled")
	}
}

func TestRequiredCompanionMissingMeansNoJob(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", dbPath, "1")
	m := metaFor("needs-wal", "1.0.0")
	m.Inputs[1].Required = true
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("needs-wal"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHost(t, c, r, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Parser.Meta().Name != "p" {
		t.Fatalf("jobs %+v, want only the parser that needs no wal", jobs)
	}
	putFile(t, c, "D1", "A1", walPath, "w")
	if jobs, _ = discover(t, h, artparse.Selection{}); len(jobs) != 2 {
		t.Errorf("with the wal present: %d jobs, want 2", len(jobs))
	}
}

func TestDiscoveryOrderIsRegistryThenArtifactID(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	for _, p := range []string{"a", "b", "c", "d"} {
		putFile(t, c, "D1", "A1", "/data/data/com."+p+"/databases/app.db", p)
		putFile(t, c, "D1", "A1", "/data/data/com."+p+"/databases/other.db", p)
	}
	zeta := register(t, "zeta", "1.0.0", "android:/data/data/*/databases/other.db")
	alpha := register(t, "alpha", "1.0.0")
	jobs, _ := discover(t, newHost(t, c, zeta, alpha), artparse.Selection{})
	if len(jobs) != 8 {
		t.Fatalf("%d jobs", len(jobs))
	}
	for i, j := range jobs {
		if j.N != i+1 {
			t.Errorf("job %d has N %d", i, j.N)
		}
		wantParser := "zeta"
		if i >= 4 {
			wantParser = "alpha"
		}
		if got := j.Parser.Meta().Name; got != wantParser {
			t.Errorf("job %d is parser %s, want %s (registry order, not name order)", i, got, wantParser)
		}
	}
	for _, half := range [][]artparse.Job{jobs[:4], jobs[4:]} {
		if ids := jobIDs(half); !slices.IsSorted(ids) {
			t.Errorf("artifact ids not ascending: %v", ids)
		}
	}
}

func TestDiscoveryNeverOpensArtifacts(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", dbPath, "1")
	putFile(t, c, "D1", "A1", walPath, "w")
	h := newHost(t, c, register(t, "p", "1.0.0"))
	snap := snapshotOf(t, h)
	if err := os.RemoveAll(filepath.Join(c.Dir, "artifacts")); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 || jobs[0].Others["wal"].Artifact.ID == "" {
		t.Fatalf("Discover without artifact files: %d jobs, %v", len(jobs), err)
	}
}

func TestExplicitArtifactMakesOneJobPerParser(t *testing.T) {
	c := newCase(t)
	// a file of a device with no evidence: no logical path, so only --artifact can name it
	odd := putFile(t, c, "D9", "A1", "/somewhere/else.bin", "x")
	h := newHost(t, c, register(t, "p", "1.0.0"), register(t, "q", "1.0.0", "android:/data/data/*/databases/q.db"))
	if jobs, _ := discover(t, h, artparse.Selection{}); len(jobs) != 0 {
		t.Fatalf("an artifact without a logical path was discovered: %+v", jobs)
	}
	jobs, _ := discover(t, h, artparse.Selection{Artifacts: []string{odd.ID}})
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want one per parser", len(jobs))
	}
	for i, j := range jobs {
		if !j.Explicit || j.Status != "new" || j.Primary.Artifact.ID != odd.ID || j.Primary.Role != "db" || j.Primary.Logical != "" || j.N != i+1 {
			t.Errorf("job %d = %+v", i, j)
		}
	}
	only, _ := discover(t, h, artparse.Selection{Artifacts: []string{odd.ID}, Parsers: []artparse.ParserRef{{Name: "q"}}})
	if len(only) != 1 || only[0].Parser.Meta().Name != "q" {
		t.Errorf("--parser q gave %+v", only)
	}
	// an eligible artifact named explicitly skips the glob test (it does not match this parser's glob)
	match := putFile(t, c, "D9", "A1", "/data/data/com.a/databases/app.db", "x")
	h2 := newHost(t, c, register(t, "q", "1.0.0", "android:/data/data/*/databases/q.db"))
	jobs, _ = discover(t, h2, artparse.Selection{Artifacts: []string{match.ID}})
	if len(jobs) != 1 || !jobs[0].Explicit {
		t.Errorf("explicit job for a non-matching eligible artifact: %+v", jobs)
	}
	// errors
	_, _, err := h.Discover(context.Background(), snapshotOf(t, h), artparse.Selection{Artifacts: []string{"no-such-id"}})
	if !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestExplicitJobBundlesCompanionsWhenNamed(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", "/odd/place/app.db", "x")
	wal := putFile(t, c, "D1", "A1", "/odd/place/app.db-wal", "w")
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{Artifacts: []string{db.ID}})
	if len(jobs) != 1 || jobs[0].Others["wal"].Artifact.ID != wal.ID {
		t.Errorf("explicit job did not bundle the companion of a named artifact: %+v", jobs)
	}
}

func TestExplicitJobWithMissingRequiredRoleIsUnparsed(t *testing.T) {
	c := newCase(t)
	odd := putFile(t, c, "D9", "A1", "/x/y", "x")
	m := metaFor("needs-wal", "1.0.0")
	m.Inputs[1].Required = true
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("needs-wal"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := discover(t, newHost(t, c, r), artparse.Selection{Artifacts: []string{odd.ID}})
	if len(jobs) != 1 || jobs[0].Status != "unparsed" || !strings.Contains(jobs[0].Reason, "wal") {
		t.Errorf("jobs %+v", jobs)
	}
}

func TestSelectionUnknownParserIsErrSelection(t *testing.T) {
	c := newCase(t)
	h := newHost(t, c, register(t, "p", "1.0.0"))
	snap := snapshotOf(t, h)
	for name, refs := range map[string][]artparse.ParserRef{
		"unknown name":      {{Name: "nope"}},
		"version mismatch":  {{Name: "p", Version: "9.9.9"}},
		"one good, one bad": {{Name: "p"}, {Name: "q", Version: "1.0.0"}},
	} {
		if _, _, err := h.Discover(context.Background(), snap, artparse.Selection{Parsers: refs}); !errors.Is(err, artparse.ErrSelection) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, refs := range [][]artparse.ParserRef{{{Name: "p"}}, {{Name: "p", Version: "1.0.0"}}} {
		if _, _, err := h.Discover(context.Background(), snap, artparse.Selection{Parsers: refs}); err != nil {
			t.Errorf("%+v: %v", refs, err)
		}
	}
}

func TestNonLiveKindsRefusedBeforeWriterRule(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	live := putFile(t, c, "D1", "A1", dbPath, "x")
	kinds := []string{"partition", "serial", "info", "import", "runs", "unallocated", "recover", "carve", "slack", "journal", "report", "", "future-kind"}
	ids := map[string]string{}
	for _, k := range kinds {
		ids[k] = put(t, c, "D1", "A1", "k-"+k+"x", evidence.Source{Kind: k, RemotePath: dbPath}, "x").ID
	}
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, counts := discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Primary.Artifact.ID != live.ID {
		t.Fatalf("jobs %v, want only the live file", jobIDs(jobs))
	}
	if counts.NotParserInput != len(kinds) || counts.Manifest != len(kinds)+1 {
		t.Errorf("counts %+v, want %d not parser input of %d", counts, len(kinds), len(kinds)+1)
	}
	for _, k := range kinds {
		_, _, err := h.Discover(context.Background(), snapshotOf(t, h), artparse.Selection{Artifacts: []string{ids[k]}})
		if !errors.Is(err, artparse.ErrNotParserInput) {
			t.Errorf("kind %q named with --artifact: %v", k, err)
		}
	}
}

func TestRecoveredArtifactsNotDiscoveredByDefault(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	d := &evidence.Derivation{ParentID: "p", FSType: "ext4", FSPath: dbPath}
	rec := put(t, c, "D1", "A1", "rec", evidence.Source{Kind: "recover", RemotePath: dbPath, Derived: d}, "x")
	carved := put(t, c, "D1", "A1", "carved", evidence.Source{Kind: "carve", RemotePath: dbPath, Derived: d}, "x")
	h := newHost(t, c, register(t, "p", "1.0.0"))
	for _, sel := range []artparse.Selection{{}, {IncludeSnapshots: true}} {
		jobs, counts := discover(t, h, sel)
		if len(jobs) != 0 || counts.NotParserInput != 2 {
			t.Errorf("%+v: jobs %v counts %+v", sel, jobIDs(jobs), counts)
		}
		for _, id := range []string{rec.ID, carved.ID} {
			if _, _, err := h.Discover(context.Background(), snapshotOf(t, h), artparse.Selection{Artifacts: []string{id}}); !errors.Is(err, artparse.ErrNotParserInput) {
				t.Errorf("%+v: --artifact %s: %v", sel, id, err)
			}
		}
	}
}

func TestSnapshotArtifactsExcludedByDefault(t *testing.T) {
	c := newCase(t)
	live := putExtract(t, c, "D1", "A1", "live", "ext4", dbPath, nil)
	snapRec := putExtract(t, c, "D1", "A1", "snap", "ext4", dbPath, &evidence.SnapshotRef{Name: "s1", Xid: 7})
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, counts := discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Primary.Artifact.ID != live.ID || jobs[0].Primary.Snapshot != nil {
		t.Fatalf("jobs %+v", jobs)
	}
	if counts.SnapshotExcluded != 1 || counts.NotParserInput != 0 || counts.Manifest != 2 {
		t.Errorf("counts %+v", counts)
	}
	_, _, err := h.Discover(context.Background(), snapshotOf(t, h), artparse.Selection{Artifacts: []string{snapRec.ID}})
	if !errors.Is(err, artparse.ErrNotParserInput) {
		t.Errorf("--artifact naming a snapshot artifact: %v", err)
	}
}

func TestIncludeSnapshotsDiscoversWithSnapshotRef(t *testing.T) {
	c := newCase(t)
	live := putExtract(t, c, "D1", "A1", "live", "ext4", dbPath, nil)
	s1 := putExtract(t, c, "D1", "A1", "snap1", "ext4", dbPath, &evidence.SnapshotRef{Name: "s1", Xid: 7})
	s2 := putExtract(t, c, "D1", "A1", "snap2", "ext4", dbPath, &evidence.SnapshotRef{Name: "s2", Xid: 9})
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, counts := discover(t, h, artparse.Selection{IncludeSnapshots: true})
	if len(jobs) != 3 || counts.SnapshotExcluded != 0 {
		t.Fatalf("%d jobs, counts %+v", len(jobs), counts)
	}
	got := map[string]*parse.SnapshotInfo{}
	for _, j := range jobs {
		got[j.Primary.Artifact.ID] = j.Primary.Snapshot
	}
	if got[live.ID] != nil {
		t.Errorf("live job carries a snapshot: %+v", got[live.ID])
	}
	if sn := got[s1.ID]; sn == nil || sn.Name != "s1" || sn.Xid != 7 {
		t.Errorf("snapshot 1 = %+v", sn)
	}
	if sn := got[s2.ID]; sn == nil || sn.Name != "s2" || sn.Xid != 9 {
		t.Errorf("snapshot 2 = %+v", sn)
	}
}

func TestBundleNeverMixesSnapshots(t *testing.T) {
	c := newCase(t)
	snap := &evidence.SnapshotRef{Name: "s1", Xid: 7}
	liveDB := putExtract(t, c, "D1", "A1", "live-db", "ext4", dbPath, nil)
	snapWAL := putExtract(t, c, "D1", "A1", "snap-wal", "ext4", walPath, snap)
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{IncludeSnapshots: true})
	if len(jobs) != 1 || len(jobs[0].Others) != 0 {
		t.Fatalf("a snapshot wal was bundled with a live db: %+v", jobs)
	}
	liveWAL := putExtract(t, c, "D1", "A1", "live-wal", "ext4", walPath, nil)
	snapDB := putExtract(t, c, "D1", "A1", "snap-db", "ext4", dbPath, snap)
	jobs, _ = discover(t, h, artparse.Selection{IncludeSnapshots: true})
	if len(jobs) != 2 {
		t.Fatalf("%d jobs", len(jobs))
	}
	for _, j := range jobs {
		switch j.Primary.Artifact.ID {
		case liveDB.ID:
			if j.Others["wal"].Artifact.ID != liveWAL.ID {
				t.Errorf("live db bundled with %+v", j.Others["wal"].Artifact.ID)
			}
		case snapDB.ID:
			if j.Others["wal"].Artifact.ID != snapWAL.ID || j.Others["wal"].Snapshot == nil || j.Others["wal"].Snapshot.Xid != 7 {
				t.Errorf("snapshot db bundled with %+v", j.Others["wal"])
			}
		default:
			t.Errorf("unexpected job %v", j.Primary.Artifact.ID)
		}
	}
}

func TestDiscoveryAndroidGlobNeverMatchesIOSPull(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "phone", "A1", "ios.backup")
	putFile(t, c, "phone", "A1", dbPath, "x")
	jobs, counts := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
	if len(jobs) != 0 || counts.Manifest != 1 {
		t.Errorf("an Android glob matched an iOS pull: %+v %+v", jobs, counts)
	}
}

func TestJobStatusFromRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered func(db, wal string) []string // nil = no run
		want    string
	}{
		{"no run", nil, "new"},
		{"run covered both", func(db, wal string) []string { return []string{db, wal} }, "already-parsed"},
		{"run covered only the db", func(db, _ string) []string { return []string{db} }, "stale-bundle"},
	} {
		c := newCase(t)
		acquireStart(t, c, "D1", "A1", "logical")
		db := putFile(t, c, "D1", "A1", dbPath, "1")
		wal := putFile(t, c, "D1", "A1", walPath, "w")
		r := register(t, "p", "1.0.0")
		if tc.covered != nil {
			recs := []records.Record{{Type: message.Type, ArtifactID: db.ID, Summary: "s", Payload: recordstest.ValidPayload(message.Type)}}
			recordstest.Ingest(t, c, r.Identity(), tc.covered(db.ID, wal.ID), recs)
		}
		jobs, _ := discover(t, newHost(t, c, r), artparse.Selection{})
		if len(jobs) != 1 || jobs[0].Status != tc.want {
			t.Errorf("%s: jobs %+v, want status %s", tc.name, jobs, tc.want)
		}
	}
}

func TestPrimaryIsNeverItsOwnCompanion(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "1")
	sibling := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/app2.db", "2")
	m := metaFor("sib", "1.0.0", "android:/data/data/*/databases/app.db")
	m.Inputs = append(m.Inputs, parse.InputSpec{Role: "sibling", Globs: []string{"./*.db"}})
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("sib"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := discover(t, newHost(t, c, r), artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Primary.Artifact.ID != db.ID || jobs[0].Status != "new" || jobs[0].Others["sibling"].Artifact.ID != sibling.ID {
		t.Fatalf("jobs %+v", jobs)
	}
}

func TestMissingRequiredRoleIsCountedPerParser(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", dbPath, "1")
	putFile(t, c, "D1", "A1", "/data/data/com.b/databases/app.db", "2")
	needs := func(name string) artparse.Registered {
		m := metaFor(name, "1.0.0")
		m.Inputs[1].Required = true
		r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor(name))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	h := newHost(t, c, needs("needs-wal"), needs("also-needs-wal"), register(t, "p", "1.0.0"))
	jobs, counts := discover(t, h, artparse.Selection{})
	if len(jobs) != 2 {
		t.Fatalf("jobs %d, want only the parser that needs no wal (twice)", len(jobs))
	}
	want := map[string]int{"needs-wal": 2, "also-needs-wal": 2}
	if len(counts.MissingRole) != 2 || counts.MissingRole["needs-wal"] != 2 || counts.MissingRole["also-needs-wal"] != 2 {
		t.Errorf("MissingRole = %v, want %v", counts.MissingRole, want)
	}
	// once the wal exists for one database the count drops for that bundle only
	putFile(t, c, "D1", "A1", walPath, "w")
	h = newHost(t, c, needs("needs-wal"))
	_, counts = discover(t, h, artparse.Selection{})
	if counts.MissingRole["needs-wal"] != 1 {
		t.Errorf("after one wal: MissingRole = %v, want needs-wal:1", counts.MissingRole)
	}
	// an explicit job with a missing role is reported as unparsed, not counted as dropped
	h = newHost(t, c, needs("also-needs-wal"))
	_, counts = discover(t, h, artparse.Selection{Artifacts: []string{jobsPrimary(t, c)}})
	if len(counts.MissingRole) != 0 {
		t.Errorf("an explicit job was counted as dropped: %v", counts.MissingRole)
	}
	// nothing missing: the map is empty, not nil-dependent
	_, counts = discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
	if len(counts.MissingRole) != 0 {
		t.Errorf("MissingRole = %v with every role present", counts.MissingRole)
	}
}

// jobsPrimary returns the id of the artifact at dbPath.
func jobsPrimary(t testing.TB, c *evidence.Case) string {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Source.RemotePath == dbPath {
			return r.ID
		}
	}
	t.Fatal("no artifact at " + dbPath)
	return ""
}

// The acquisition of an artifact is the layout evidence.Case.NewArtifact
// builds, artifacts/<device>/<acquisition>/<rest>, with sanitised components
// that never hold a slash. The test builds artifacts through NewArtifact and
// Capture, so a change of that layout fails here.
func TestAcquisitionKeyFollowsTheNewArtifactLayout(t *testing.T) {
	c := newCase(t)
	capture := func(dev, acq, rel string) string {
		t.Helper()
		return putFile(t, c, dev, acq, "/"+rel, "x").Path
	}
	newArtifact := func(dev, acq, rel string) string {
		t.Helper()
		w, err := c.NewArtifact(dev, acq, rel, evidence.Source{Kind: "file", DeviceID: dev, RemotePath: "/" + rel})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		rec, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		return rec.Path
	}
	same := []string{
		capture("D1", "A1", "data/a.db"),
		capture("D1", "A1", "data/deep/er/b.db"),
		newArtifact("D1", "A1", "top.db"),
	}
	key := mustKey(t, same[0])
	for _, p := range same {
		if got := mustKey(t, p); got != key {
			t.Errorf("%s: key %q, want %q (same device and acquisition)", p, got, key)
		}
	}
	if !strings.HasPrefix(same[0], key+"/") {
		t.Errorf("key %q is not a directory prefix of %q", key, same[0])
	}
	// other acquisition, other device, and components holding slashes must not merge
	others := []string{
		capture("D1", "A2", "data/a.db"),
		capture("D2", "A1", "data/a.db"),
		newArtifact("D/1", "A", "x.db"),
		newArtifact("D", "1/A", "x.db"),
	}
	seen := map[string]string{key: same[0]}
	for _, p := range others {
		k := mustKey(t, p)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share the acquisition key %q", p, prev, k)
		}
		seen[k] = p
	}
}
