package artparse_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// fakeSource serves a crafted manifest and audit log to a host.
type fakeSource struct {
	recs  []evidence.ManifestRecord
	audit []evidence.AuditEntry
}

func (s fakeSource) Manifest() ([]evidence.ManifestRecord, error) { return s.recs, nil }
func (s fakeSource) ReadAudit() ([]evidence.AuditEntry, error)    { return s.audit, nil }

func mustKey(t testing.TB, p string) string {
	t.Helper()
	k, ok := artparse.AcquisitionKey(p)
	if !ok {
		t.Fatalf("%q has no acquisition key", p)
	}
	return k
}

func mrec(id, path string, src evidence.Source) evidence.ManifestRecord {
	return evidence.ManifestRecord{ID: id, Path: path, Size: 1, SHA256: strings.Repeat("a", 64), Source: src}
}

func startEntry(action, dev, typ string) evidence.AuditEntry {
	return evidence.AuditEntry{Action: action, DeviceID: dev, Details: map[string]any{"type": typ}}
}

func fakeHost(t *testing.T, src fakeSource, ps ...artparse.Registered) (*artparse.Host, *artparse.Snapshot) {
	t.Helper()
	opt := artparse.Options{Limits: parse.DefaultLimits()}
	artparse.WithCaseSource(&opt, src)
	h, err := artparse.New(newCase(t), ps, opt)
	if err != nil {
		t.Fatal(err)
	}
	return h, snapshotOf(t, h)
}

func TestUnnamedEligibleArtifactsAreCountedAndListed(t *testing.T) {
	src := fakeSource{
		audit: []evidence.AuditEntry{startEntry("acquire.start", "D1", "logical")},
		recs: []evidence.ManifestRecord{
			mrec("a1", "artifacts/D1/A1/files/data/data/com.a/databases/app.db", evidence.Source{Kind: "file", DeviceID: "D1", RemotePath: dbPath}),
			mrec("b1", "artifacts/D9/A1/files/x/y", evidence.Source{Kind: "file", DeviceID: "D9", RemotePath: "/x/y"}),
			mrec("c1", "artifacts/D1/A1/ex/q", evidence.Source{Kind: "extract", DeviceID: "D1", Derived: &evidence.Derivation{FSType: "xfs", FSPath: "/q"}}),
			mrec("d1", "artifacts/D1/A1/bk/Manifest.db", evidence.Source{Kind: "backup", DeviceID: "D1", RemotePath: "Manifest.db"}),
			mrec("e1", "artifacts/D1/A1/info/x", evidence.Source{Kind: "info", DeviceID: "D1", RemotePath: "exec:getprop"}),
		},
	}
	h, snap := fakeHost(t, src, register(t, "p", "1.0.0"))
	_, counts, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	env := snap.Env()
	want := []artparse.UnnamedArtifact{
		{ArtifactID: "b1", Path: "artifacts/D9/A1/files/x/y", Reason: artparse.WhyNoLogical(src.recs[1], env)},
		{ArtifactID: "c1", Path: "artifacts/D1/A1/ex/q", Reason: artparse.WhyNoLogical(src.recs[2], env)},
		{ArtifactID: "d1", Path: "artifacts/D1/A1/bk/Manifest.db", Reason: artparse.WhyNoLogical(src.recs[3], env)},
	}
	if !slices.Equal(counts.UnnamedList, want) {
		t.Errorf("UnnamedList = %+v\nwant %+v", counts.UnnamedList, want)
	}
	if len(counts.Unnamed) != 3 {
		t.Errorf("Unnamed = %v, want one count for each of the three reasons", counts.Unnamed)
	}
	for _, w := range want {
		if w.Reason == "" || counts.Unnamed[w.Reason] != 1 {
			t.Errorf("reason %q counted %d times, want 1", w.Reason, counts.Unnamed[w.Reason])
		}
	}
	if counts.NotParserInput != 1 {
		t.Errorf("NotParserInput = %d, want the info artifact only", counts.NotParserInput)
	}
}

func TestAcquisitionKeyFailsClosed(t *testing.T) {
	for p, want := range map[string]struct {
		key string
		ok  bool
	}{
		"x":                         {"", false},
		"artifacts":                 {"", false},
		"artifacts/x":               {"", false},
		"artifacts/D1/A1":           {"", false},
		"artifacts/D1/A1/":          {"", false},
		"artifacts/D1/A1/f":         {"artifacts/D1/A1", true},
		"artifacts/D1/A1/deep/er/f": {"artifacts/D1/A1", true},
		"other/D1/A1/f":             {"", false},
		"/artifacts/D1/A1/f":        {"", false},
		"artifacts//A1/f":           {"", false},
		"artifacts/D1//f":           {"", false},
		"":                          {"", false},
	} {
		if k, ok := artparse.AcquisitionKey(p); k != want.key || ok != want.ok {
			t.Errorf("AcquisitionKey(%q) = %q, %v; want %q, %v", p, k, ok, want.key, want.ok)
		}
	}
}

func TestPathOutsideTheAcquisitionLayoutIsNeverBundled(t *testing.T) {
	file := func(id, path, remote string) evidence.ManifestRecord {
		return mrec(id, path, evidence.Source{Kind: "file", DeviceID: "D1", RemotePath: remote})
	}
	audit := []evidence.AuditEntry{startEntry("acquire.start", "D1", "logical")}
	for name, paths := range map[string][2]string{
		"depth 1": {"x", "y"}, "depth 2": {"artifacts/x", "artifacts/y"}, "depth 3": {"artifacts/D1/A1", "artifacts/D1/A2"},
		"first component": {"other/D1/A1/f", "other/D1/A1/g"},
	} {
		t.Run(name, func(t *testing.T) {
			h, snap := fakeHost(t, fakeSource{audit: audit, recs: []evidence.ManifestRecord{
				file("db", paths[0], dbPath), file("wal", paths[1], walPath),
			}}, register(t, "p", "1.0.0"))
			jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || jobs[0].Status != artparse.StatusUnparsed ||
				!strings.Contains(jobs[0].Reason, "manifest path outside the acquisition layout") || len(jobs[0].Others) != 0 {
				t.Errorf("jobs %+v, want one unparsed job with no companion", jobs)
			}
		})
	}
	t.Run("depth 4 is the layout", func(t *testing.T) {
		h, snap := fakeHost(t, fakeSource{audit: audit, recs: []evidence.ManifestRecord{
			file("db", "artifacts/D1/A1/f", dbPath), file("wal", "artifacts/D1/A1/g", walPath),
		}}, register(t, "p", "1.0.0"))
		jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
		if err != nil || len(jobs) != 1 || jobs[0].Status != artparse.StatusNew || jobs[0].Others["wal"].Artifact.ID != "wal" {
			t.Errorf("jobs %+v, err %v", jobs, err)
		}
	})
}

func TestExplicitArtifactsCollapseAndAreSorted(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	var idList []string
	for _, a := range []string{"a", "b", "c"} {
		idList = append(idList, putFile(t, c, "D1", "A1", "/data/data/com."+a+"/databases/app.db", a).ID)
	}
	slices.Sort(idList)
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{Artifacts: []string{idList[2], idList[0], idList[2], idList[1], idList[0]}})
	if got := jobIDs(jobs); !slices.Equal(got, idList) {
		t.Errorf("explicit jobs over %v, want each artifact once, sorted: %v", got, idList)
	}
	for i, j := range jobs {
		if j.N != i+1 {
			t.Errorf("job %d has N=%d", i, j.N)
		}
	}
}

func TestCompanionSuffixMatchesByEquality(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "1")
	putFile(t, c, "D1", "A1", dbPath+"-wal2", "x")
	putFile(t, c, "D1", "A1", dbPath+"-walx", "x")
	putFile(t, c, "D1", "A1", dbPath+"-wa", "x")
	h := newHost(t, c, register(t, "p", "1.0.0"))
	jobs, _ := discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Primary.Artifact.ID != db.ID || len(jobs[0].Others) != 0 {
		t.Fatalf("jobs %+v: -wal2, -walx and -wa are not the -wal companion", jobs)
	}
	wal := putFile(t, c, "D1", "A1", walPath, "w")
	jobs, _ = discover(t, h, artparse.Selection{})
	if len(jobs) != 1 || jobs[0].Others["wal"].Artifact.ID != wal.ID {
		t.Errorf("the exact -wal was not bundled: %+v", jobs)
	}
}

func TestPlatformOfAnUnnamedMember(t *testing.T) {
	src := fakeSource{
		audit: []evidence.AuditEntry{startEntry("acquire.start", "DA", "logical")},
		recs: []evidence.ManifestRecord{
			mrec("bk", "artifacts/DA/A1/bk/f", evidence.Source{Kind: "backup", DeviceID: "DA", RemotePath: "x"}),
			mrec("fa", "artifacts/DA/A1/fa/f", evidence.Source{Kind: "file", DeviceID: "DA", RemotePath: "relative/x"}),
			mrec("fu", "artifacts/DU/A1/fu/f", evidence.Source{Kind: "file", DeviceID: "DU", RemotePath: "relative/x"}),
		},
	}
	h, snap := fakeHost(t, src, register(t, "p", "1.0.0"))
	for id, want := range map[string]string{"bk": parse.PlatformIOS, "fa": parse.PlatformAndroid, "fu": ""} {
		jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{Artifacts: []string{id}})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("%s: jobs %v err %v", id, jobs, err)
		}
		if got := jobs[0].Primary.Platform; got != want {
			t.Errorf("%s: platform %q, want %q", id, got, want)
		}
	}
}

func TestBuildEnvFilters(t *testing.T) {
	src := fakeSource{
		audit: []evidence.AuditEntry{
			startEntry("acquire.start", "A", "logical"),
			startEntry("acquire.end", "B", "logical"),
			startEntry("acquire.start", "C", "other"),
			startEntry("acquire.start", "X", "x"),
			startEntry("acquire.start", "", "logical"),
			startEntry("acquire.start", "I", "ios.backup"),
			startEntry("acquire.start", "BOTH", "logical"),
			startEntry("acquire.start", "BOTH", "ios.backup"),
		},
		recs: []evidence.ManifestRecord{
			mrec("1", "artifacts/E/A/i1", evidence.Source{Kind: "info", DeviceID: "E", RemotePath: "exec:getprop"}),
			mrec("2", "artifacts/S/A/i2", evidence.Source{Kind: "info", DeviceID: "S", RemotePath: "shell:ls"}),
			mrec("3", "artifacts/F/A/i3", evidence.Source{Kind: "file", DeviceID: "F", RemotePath: "exec:getprop"}),
			mrec("4", "artifacts/G/A/i4", evidence.Source{Kind: "info", DeviceID: "G", RemotePath: "lockdown:values"}),
			mrec("5", "artifacts/H/A/i5", evidence.Source{Kind: "info", DeviceID: "H", RemotePath: "lockdownvalues"}),
			mrec("6", "artifacts/N/A/i6", evidence.Source{Kind: "info", DeviceID: "N", RemotePath: "other:x"}),
			mrec("7", "artifacts/Z/A/i7", evidence.Source{Kind: "info", DeviceID: "", RemotePath: "exec:getprop"}),
			mrec("8", "artifacts/LK/A/i8", evidence.Source{Kind: "info", DeviceID: "LK", RemotePath: "lockdown:a"}),
			mrec("9", "artifacts/LK/A/i9", evidence.Source{Kind: "info", DeviceID: "LK", RemotePath: "exec:b"}),
		},
	}
	_, snap := fakeHost(t, src)
	want := map[string]string{
		"A": parse.PlatformAndroid, "I": parse.PlatformIOS, "E": parse.PlatformAndroid, "S": parse.PlatformAndroid, "G": parse.PlatformIOS,
	}
	got := snap.Env().DevicePlatform
	if len(got) != len(want) {
		t.Errorf("DevicePlatform = %v, want %v", got, want)
	}
	for dev, p := range want {
		if got[dev] != p {
			t.Errorf("device %s: %q, want %q", dev, got[dev], p)
		}
	}
	for _, dev := range []string{"B", "C", "X", "", "F", "H", "N", "BOTH", "LK"} {
		if p, ok := got[dev]; ok {
			t.Errorf("device %q has platform %q, want none", dev, p)
		}
	}
}

func TestEncryptedContentMakesTheJobUnparsed(t *testing.T) {
	enc := func(c *evidence.Case, rel, fsPath string, encrypted bool) evidence.ManifestRecord {
		d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: fsPath, Encrypted: encrypted}
		return put(t, c, "D1", "A1", rel, evidence.Source{Kind: "extract", RemotePath: fsPath, Derived: d}, "x")
	}
	t.Run("primary", func(t *testing.T) {
		c := newCase(t)
		enc(c, "a/db", dbPath, true)
		jobs, _ := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
		if len(jobs) != 1 || jobs[0].Status != artparse.StatusUnparsed || !strings.Contains(jobs[0].Reason, "encrypted content") {
			t.Errorf("jobs %+v", jobs)
		}
	})
	t.Run("companion", func(t *testing.T) {
		c := newCase(t)
		enc(c, "a/db", dbPath, false)
		enc(c, "a/wal", walPath, true)
		jobs, _ := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
		if len(jobs) != 1 || jobs[0].Status != artparse.StatusUnparsed || !strings.Contains(jobs[0].Reason, "encrypted content") {
			t.Errorf("jobs %+v", jobs)
		}
	})
	t.Run("explicit", func(t *testing.T) {
		c := newCase(t)
		rec := enc(c, "a/db", dbPath, true)
		jobs, _ := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{Artifacts: []string{rec.ID}})
		if len(jobs) != 1 || jobs[0].Status != artparse.StatusUnparsed || !strings.Contains(jobs[0].Reason, "encrypted content") {
			t.Errorf("jobs %+v", jobs)
		}
	})
	t.Run("not encrypted", func(t *testing.T) {
		c := newCase(t)
		enc(c, "a/db", dbPath, false)
		jobs, _ := discover(t, newHost(t, c, register(t, "p", "1.0.0")), artparse.Selection{})
		if len(jobs) != 1 || jobs[0].Status != artparse.StatusNew {
			t.Errorf("jobs %+v", jobs)
		}
	})
}
