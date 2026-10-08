package evidence_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// FA-m1: an artifact that records excluded runs or non-zero excluded counters must be flagged incomplete.
func TestVerifyExcludedRunsRequireIncomplete(t *testing.T) {
	const want = "excluded runs recorded but the artifact is not flagged incomplete"
	for name, tc := range map[string]struct {
		mutate     func(*evidence.Recovery)
		incomplete bool
		wantProb   bool
	}{
		"excluded run listed":    {func(r *evidence.Recovery) { r.Excluded = []evidence.Run{{Offset: 64, Length: 8}} }, false, true},
		"excluded runs counter":  {func(r *evidence.Recovery) { r.Alloc.ExcludedRuns = 3 }, false, true},
		"excluded bytes counter": {func(r *evidence.Recovery) { r.Alloc.ExcludedBytes = 24 }, false, true},
		"listed and incomplete": {func(r *evidence.Recovery) {
			r.Excluded = []evidence.Run{{Offset: 64, Length: 8}}
			r.Alloc.ExcludedRuns, r.Alloc.ExcludedBytes = 1, 8
		}, true, false},
		"nothing excluded": {func(*evidence.Recovery) {}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
				Runs: []evidence.Run{{Offset: 0, Length: 64}}, Incomplete: tc.incomplete, Mutate: tc.mutate,
			})
			rep := verifyOf(t, c)
			if tc.wantProb {
				evidencetest.RequireProblem(t, rep, want)
			} else {
				evidencetest.RequireNoProblem(t, rep, want)
			}
		})
	}
}

// FA-m2: inline runs and a runs sidecar together are ambiguous provenance.
func TestVerifyInlineRunsAndSidecarTogetherIsAProblem(t *testing.T) {
	c, parent, data := recoverParent(t)
	sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
	dd := derivedOf(parent, evidence.Run{Offset: 0, Length: 64})
	dd.RunsArtifact = sc.ID
	dd.Recovery = validRecovery(64)
	rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, data[:64], nil)
	evidencetest.RequireProblem(t, verifyOf(t, c), "records both inline runs and a runs sidecar")
}

// FA-m3: the path scope of a carved artifact is tied to Recovery.Scope, and a slack or journal artifact
// lives under the directory of its own partition (spec 5.1).
func TestVerifyScopeDirectoryIsTiedToTheRecovery(t *testing.T) {
	carved := func(scope, path string) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{
			Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-jpeg", Scope: scope, Path: path,
			Runs: []evidence.Run{{Offset: 0, Length: 64}},
		}
	}
	slack := func(path string) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{
			Kind: evidence.KindSlack, Class: evidence.ClassSlack, Method: "slack-file", Path: path, Confidence: ip(10),
			Runs: []evidence.Run{{Offset: 0, Length: 64}},
		}
	}
	for _, tc := range []struct {
		name string
		spec evidencetest.RecoveredSpec
		want string // "" = no scope-directory problem
	}{
		{"raw in raw", carved("raw", "carved/raw/c1.bin"), ""},
		{"volume in volume", carved("volume", "carved/volume/c1.bin"), ""},
		{"unallocated in its partition", carved("unallocated", "carved/p1-mtfs/c1.bin"), ""},
		{"artifact in a spec scope directory", carved("artifact", "carved/raw/c1.bin"), ""},
		{"raw in a partition directory", carved("raw", "carved/p1-mtfs/c1.bin"), `scope directory "p1-mtfs" does not match the recovery scope "raw"`},
		{"unallocated in raw", carved("unallocated", "carved/raw/c1.bin"), `scope directory "raw" does not match the recovery scope "unallocated"`},
		{"unallocated in another partition", carved("unallocated", "carved/p2-mtfs/c1.bin"), `scope directory "p2-mtfs" does not match the recovery scope "unallocated"`},
		{"volume in raw", carved("volume", "carved/raw/c1.bin"), `scope directory "raw" does not match the recovery scope "volume"`},
		{"artifact in a made-up directory", carved("artifact", "carved/zzz/c1.bin"), `scope directory "zzz" is not one of raw, volume or p<N>-<fstype>`},
		{"carved without a scope directory", carved("raw", "carved/c1.bin"), `has no scope directory`},
		{"slack of its partition", slack("slack/p1-mtfs/slack.bin"), ""},
		{"slack of another partition", slack("slack/p2-mtfs/slack.bin"), `path directory "p2-mtfs" does not match derivation partition 1 ("mtfs")`},
		{"slack without a directory", slack("slack/slack.bin"), `has no partition directory`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, tc.spec)
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "scope directory")
				evidencetest.RequireNoProblem(t, rep, "path directory")
				evidencetest.RequireNoProblem(t, rep, "has no ")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

// FA-m4: a nil index is an error, never a panic.
func TestOpenArtifactInNilIndex(t *testing.T) {
	c := evidencetest.NewCase(t)
	f, _, err := c.OpenArtifactIn(nil, "x")
	if err == nil || f != nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("OpenArtifactIn(nil) = %v, %v; want an error naming the index", f, err)
	}
}
