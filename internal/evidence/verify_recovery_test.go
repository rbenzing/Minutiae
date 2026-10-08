package evidence_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// recoverParent is the parent image of the R-series tests: 16 KiB of a
// deterministic pattern, so any run inside it reproduces known bytes.
func recoverParent(t *testing.T) (*evidence.Case, evidence.ManifestRecord, []byte) {
	t.Helper()
	data := make([]byte, 16384)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	c := evidencetest.NewCase(t)
	return c, evidencetest.AddImage(t, c, data), data
}

// rawArtifact captures an artifact with a hand-made Source, as a writer with a bug would.
func rawArtifact(t *testing.T, c *evidence.Case, path string, src evidence.Source, data []byte, failure error) evidence.ManifestRecord {
	t.Helper()
	src.DeviceID = evidencetest.ImageDevice
	c.DisableRecoveredKindGate() // some tests plant the undescribed recovered kinds the gate refuses
	rec, err := c.Capture(evidencetest.ImageDevice, evidencetest.RecoveredAcq, path, src, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		return failure
	})
	if failure == nil && err != nil {
		t.Fatal(err)
	}
	return rec
}

func derivedOf(parent evidence.ManifestRecord, runs ...evidence.Run) *evidence.Derivation {
	return &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", Runs: runs}
}

func verifyOf(t *testing.T, c *evidence.Case, checks ...evidence.VerifyCheck) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), checks...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestVerifyCleanRecoveredCase(t *testing.T) {
	c, parent, data := recoverParent(t)
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 4096, Length: 512}}})
	var many []evidence.Run
	for i := range 5000 {
		many = append(many, evidence.Run{Offset: int64(i * 3), Length: 1})
	}
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000002-b.bin", Runs: many, Sidecar: true, Confidence: ip(70),
	})
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000003-c.bin", Runs: []evidence.Run{{Offset: 8192, Length: 100}},
		Incomplete: true, Mutate: func(r *evidence.Recovery) { r.Confidence = ip(20) },
	})
	rep := verifyOf(t, c)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredArtifacts != 3 {
		t.Fatalf("RecoveredArtifacts = %d, want 3", rep.RecoveredArtifacts)
	}
	evidencetest.RequireNotice(t, rep, "3 recovered artifact(s) (deleted-file 3)")
	evidencetest.RequireNotice(t, rep, "lowest confidence 20")
	evidencetest.RequireNotice(t, rep, "recovered data is not live evidence")
	if got, want := rep.RecoveredSummary(), ", 3 recovered (0 reproduced)"; got != want {
		t.Fatalf("RecoveredSummary = %q, want %q", got, want)
	}
	if got := (evidence.VerifyReport{}).RecoveredSummary(); got != "" {
		t.Fatalf("RecoveredSummary of nothing = %q, want empty", got)
	}
}

func TestVerifyRecoveredKindNeedsRecovery(t *testing.T) {
	for _, kind := range []string{"recover", "carve", "slack", "journal", "report"} {
		ns := map[string]string{"recover": "recovered/p1-mtfs/000001-a", "carve": "carved/a", "slack": "slack/a", "journal": "journal/a", "report": "reports/a"}[kind]
		t.Run(kind+"/no derivation", func(t *testing.T) {
			c, _, _ := recoverParent(t)
			rawArtifact(t, c, ns, evidence.Source{Kind: kind}, []byte("x"), nil)
			evidencetest.RequireProblem(t, verifyOf(t, c), "has no derivation (recovered artifacts need one)")
		})
		t.Run(kind+"/no recovery", func(t *testing.T) {
			c, parent, _ := recoverParent(t)
			rawArtifact(t, c, ns, evidence.Source{Kind: kind, Derived: derivedOf(parent, evidence.Run{Offset: 0, Length: 1})}, []byte("x"), nil)
			evidencetest.RequireProblem(t, verifyOf(t, c), "has no recovery description")
		})
	}
}

func TestVerifyRecoveryNeedsRecoveredKind(t *testing.T) {
	for _, kind := range []string{"extract", "unallocated", "file", "runs", "import"} {
		t.Run(kind, func(t *testing.T) {
			c, parent, _ := recoverParent(t)
			d := derivedOf(parent, evidence.Run{Offset: 0, Length: 1})
			d.Recovery = &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(50), Alloc: evidence.AllocSummary{Free: 1}, Algorithm: evidence.AlgorithmRecover}
			rawArtifact(t, c, "p1-mtfs/a.bin", evidence.Source{Kind: kind, Derived: d}, []byte("x"), nil)
			evidencetest.RequireProblem(t, verifyOf(t, c), "carries a recovery description (only kinds recover, carve, slack, journal and report may)")
		})
	}
}

func TestVerifyRecoveredClassKindMismatch(t *testing.T) {
	c, parent, data := recoverParent(t)
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Runs: []evidence.Run{{Offset: 0, Length: 64}}, Mutate: func(r *evidence.Recovery) { r.Class = evidence.ClassCarved; r.Scope = "raw" },
	})
	evidencetest.RequireProblem(t, verifyOf(t, c), `class "carved" belongs to kind "carve", not "recover"`)
}

func TestVerifyRecoveryRulesTable(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(*evidence.Recovery)
		want   string
	}{
		{"deleted-file ceiling", func(r *evidence.Recovery) { r.Confidence = ip(81) }, "recovery: confidence 81 is above the ceiling 80"},
		{"deleted-file at ceiling is fine", func(r *evidence.Recovery) { r.Confidence = ip(80) }, ""},
		{"missing confidence", func(r *evidence.Recovery) { r.Confidence = nil }, `recovery: confidence is required for class "deleted-file"`},
		{"confidence out of range", func(r *evidence.Recovery) { r.Confidence = ip(101) }, "recovery: confidence 101 is outside 0..100"},
		{"unknown method prefix", func(r *evidence.Recovery) { r.Method = "zip-magic" }, `recovery: method "zip-magic" is not allowed for class "deleted-file"`},
		{"method not a token", func(r *evidence.Recovery) { r.Method = "Fat X" }, `recovery: method "Fat X" is not a token`},
		{"empty algorithm", func(r *evidence.Recovery) { r.Algorithm = "" }, "recovery: algorithm is empty"},
		{"unknown class", func(r *evidence.Recovery) { r.Class = "made-up" }, `recovery: class "made-up" is not known`},
		{"bad scope", func(r *evidence.Recovery) { r.Scope = "everywhere" }, "recovery: scope"},
		{"bad content", func(r *evidence.Recovery) { r.Content = "plausible" }, "recovery: content"},
		{"journal on deleted-file with bad region", func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Region: "maybe"} }, "recovery: journal reference"},
		{"negative alloc", func(r *evidence.Recovery) { r.Alloc.ExcludedRuns = -1 }, "recovery: alloc"},
		{"basis text", func(r *evidence.Recovery) { r.Basis = []string{"a\x00b"} }, "recovery: basis"},
		{"params key", func(r *evidence.Recovery) { r.Params = map[string]string{"Bad Key": "x"} }, "recovery: params"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}, Mutate: row.mutate})
			rep := verifyOf(t, c)
			if row.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, row.want)
			evidencetest.RequireProblem(t, rep, `artifact "`) // the artifact prefix names id and path
		})
	}
	t.Run("carved ceiling 71", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
			Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-jpeg", Scope: "volume", Path: "carved/c1.bin",
			Runs: []evidence.Run{{Offset: 0, Length: 64}}, Confidence: ip(71),
		})
		evidencetest.RequireProblem(t, verifyOf(t, c), `recovery: confidence 71 is above the ceiling 70 of class "carved"`)
	})
}

func TestVerifyRecoveredNamespace(t *testing.T) {
	run := evidence.Run{Offset: 0, Length: 1}
	rows := []struct {
		name string
		do   func(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, data []byte)
		want string // "" = the case must be clean
	}{
		{"recover outside recovered/", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			evidencetest.AddRecovered(t, c, p, d, evidencetest.RecoveredSpec{Path: "files/000001-a.bin", Runs: []evidence.Run{run}})
		}, `is outside the namespace "recovered"`},
		{"carve inside recovered/", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			evidencetest.AddRecovered(t, c, p, d, evidencetest.RecoveredSpec{Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-jpeg", Scope: "raw", Path: "recovered/p1-mtfs/000001-a.bin", Runs: []evidence.Run{run}})
		}, `is outside the namespace "carved"`},
		{"extract planted in recovered/", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			rawArtifact(t, c, "recovered/p1-mtfs/000001-x", evidence.Source{Kind: "extract", Derived: derivedOf(p, run)}, d[:1], nil)
		}, "is in the recovered namespace"},
		{"unallocated planted in slack/", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			rawArtifact(t, c, "slack/unallocated.bin", evidence.Source{Kind: "unallocated", Derived: derivedOf(p, run)}, d[:1], nil)
		}, "is in the recovered namespace"},
		{"wrong partition directory", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			evidencetest.AddRecovered(t, c, p, d, evidencetest.RecoveredSpec{Path: "recovered/p2-mtfs/000001-a.bin", Runs: []evidence.Run{run}})
		}, "does not match derivation partition"},
		{"missing ordinal", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			evidencetest.AddRecovered(t, c, p, d, evidencetest.RecoveredSpec{Path: "recovered/p1-mtfs/a.bin", Runs: []evidence.Run{run}})
		}, "does not start with an ordinal"},
		{"orphan sidecar", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, _ []byte) {
			rawArtifact(t, c, "recovered/p1-mtfs/000009-z.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(p)}, []byte(`{"offset":0,"length":1}`+"\n"), nil)
		}, "is not referenced by any recovered artifact"},
		{"sidecar of another directory", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			sc := rawArtifact(t, c, "recovered/p1-mtfs/sub/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(p)}, []byte(`{"offset":0,"length":1}`+"\n"), nil)
			dd := derivedOf(p)
			dd.RunsArtifact = sc.ID
			dd.Recovery = &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(50), Alloc: evidence.AllocSummary{Free: 1}, Algorithm: evidence.AlgorithmRecover}
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, d[:1], nil)
		}, "is not a runs sidecar in the same directory"},
		{"import file literally named recovered", func(t *testing.T, c *evidence.Case, _ evidence.ManifestRecord, d []byte) {
			rawArtifact(t, c, "recovered", evidence.Source{Kind: "import", OriginalPath: "recovered", Segment: 1, Segments: 1}, d[:4], nil)
		}, ""},
		{"android-style files/recovered/x", func(t *testing.T, c *evidence.Case, _ evidence.ManifestRecord, d []byte) {
			rawArtifact(t, c, "files/recovered/x", evidence.Source{Kind: "adb-pull", RemotePath: "/sdcard/recovered/x"}, d[:4], nil)
		}, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			row.do(t, c, parent, data)
			rep := verifyOf(t, c)
			if row.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, row.want)
		})
	}
}

func TestVerifyRealAcquisitionLayoutsStayClean(t *testing.T) {
	c, parent, data := recoverParent(t)
	run := evidence.Run{Offset: 0, Length: 4}
	for _, p := range []string{
		"files/data/app/base.apk", "device/info.json", "backup/Manifest.db", "backup/files/recovered/x",
		"console.log", "serial/session.bin", "p1-ext4/etc/passwd", "p1-ext4/recovered/old", "p2-fat/slack/x", "p1-ext4/journal",
	} {
		rawArtifact(t, c, p, evidence.Source{Kind: "adb-pull"}, data[:4], nil)
	}
	rawArtifact(t, c, "p1-ext4/etc/shadow", evidence.Source{Kind: "extract", Derived: derivedOf(parent, run)}, data[:4], nil)
	rawArtifact(t, c, "p1-ext4/unallocated.bin", evidence.Source{Kind: "unallocated", Derived: derivedOf(parent, run)}, data[:4], nil)
	rawArtifact(t, c, "volume/unallocated.bin", evidence.Source{Kind: "unallocated", Derived: derivedOf(parent, run)}, data[:4], nil)
	rep := verifyOf(t, c)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredArtifacts != 0 {
		t.Fatalf("RecoveredArtifacts = %d, want 0", rep.RecoveredArtifacts)
	}
	for _, n := range rep.Notices {
		if strings.Contains(n, "recovered artifact(s)") {
			t.Fatalf("a case without recovered artifacts raised %q", n)
		}
	}
}
