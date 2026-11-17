//go:build realimages

package cli

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// The recovery commands over real Android and iOS samples (3K-R5). They run only with -tags realimages and
// $MINUTIAE_TEST_IMAGES; without a sample every test SKIPs with the reason. Running them is an OPERATOR step, never
// a gate (docs/testing.md "Real device samples for recovery"). The layout, its strict decoding and its refusals live
// in realimages_layout_test.go, which the default tests check. A sample may be personal data: the subtests are
// numbered, and the logs carry exit codes, counts, methods, classes and types only, never a path, a file name or
// command output.

// rxRoot is the sample root: it skips when $MINUTIAE_TEST_IMAGES is unset and fails when it is inside the repository.
func rxRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("MINUTIAE_TEST_IMAGES")
	if root == "" {
		t.Skip("MINUTIAE_TEST_IMAGES is not set")
	}
	repo, err := rxRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := rxRootError(root, repo); err != nil {
		t.Fatal(err)
	}
	return root
}

func rxImageSets(t *testing.T, root string) []rxImageSet {
	t.Helper()
	sets, err := rxLoadImageSets(root)
	if err != nil {
		t.Fatal(err)
	}
	return sets
}

func rxDBSets(t *testing.T, root string) []rxDBSet {
	t.Helper()
	sets, err := rxLoadDBSets(root)
	if err != nil {
		t.Fatal(err)
	}
	return sets
}

// rxNew is what one step added to the case.
type rxNew []evidence.ManifestRecord

func rxRecovery(r evidence.ManifestRecord) evidence.Recovery {
	if r.Source.Derived == nil || r.Source.Derived.Recovery == nil {
		return evidence.Recovery{}
	}
	return *r.Source.Derived.Recovery
}

func (n rxNew) count(keep func(evidence.ManifestRecord, evidence.Recovery) bool) int {
	c := 0
	for _, r := range n {
		if keep(r, rxRecovery(r)) {
			c++
		}
	}
	return c
}

// rxDiff is the records of after that are not in before.
func rxDiff(before, after []evidence.ManifestRecord) rxNew {
	seen := map[string]bool{}
	for _, r := range before {
		seen[r.ID] = true
	}
	var out rxNew
	for _, r := range after {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func rxTally(n rxNew, key func(evidence.Recovery) string) string {
	m := map[string]int{}
	for _, r := range n {
		m[key(rxRecovery(r))]++
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func rxGunzip(t *testing.T, src string) string {
	t.Helper()
	in, err := os.Open(src) //nolint:gosec // an operator-supplied sample
	if err != nil {
		t.Fatal(rxPathless(err))
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sample.img")
	w, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, zr); err != nil { //nolint:gosec // an operator-supplied sample
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rxStepRun runs one image command. Exit 0 is accepted; exit 1 only when the command states an undecided count or
// that the filesystem does not support recovery; any other code, exit 4 above all, fails. The output is never logged.
func rxStepRun(t *testing.T, name string, args ...string) {
	t.Helper()
	code, out := run(t, Deps{}, args...)
	switch {
	case code == 0:
		t.Logf("step %s: exit 0", name)
	case code == 1 && (strings.Contains(out, "undecided") || strings.Contains(out, "does not support recovery")):
		t.Logf("step %s: exit 1 (an undecided count or an unsupported filesystem)", name)
	default:
		t.Fatalf("step %s: exit %d (the output is not logged: it may hold personal data)", name, code)
	}
}

func TestRealImagesRecovery(t *testing.T) {
	sets := rxImageSets(t, rxRoot(t))
	if len(sets) == 0 {
		t.Skip("the layout holds no recovery set")
	}
	for i, s := range sets {
		t.Run(fmt.Sprintf("set-%d", i+1), func(t *testing.T) {
			if err := rxCheckSHA256(s.Path, s.Expect.ImageSHA256, s.Gz); err != nil {
				t.Fatal(err)
			}
			img := s.Path
			if s.Gz {
				img = rxGunzip(t, s.Path)
			}
			c := newCLICase(t)
			code, out := run(t, Deps{}, "image", "import", "--case", c, "--json", img)
			if code != 0 {
				t.Fatalf("image import: exit %d", code)
			}
			var imp []evidence.ManifestRecord
			if err := json.Unmarshal(jsonPart(out), &imp); err != nil || len(imp) == 0 {
				t.Fatalf("image import: no record (%v)", err)
			}
			ref := imp[0].ID
			var part []string
			if s.Expect.Partition != nil {
				part = []string{"-p", fmt.Sprint(*s.Expect.Partition)}
			}
			t.Logf("platform %s, partition %v, expected fstype %s", s.Expect.Platform, s.Expect.Partition, s.Expect.FSType)

			code, out = run(t, Deps{}, "image", "info", "--case", c, "--json", ref)
			if code != 0 {
				t.Fatalf("image info: exit %d", code)
			}
			var info jsonImageInfo
			if err := json.Unmarshal(jsonPart(out), &info); err != nil {
				t.Fatalf("image info json: %v", err)
			}
			found := false
			for _, p := range info.Partitions {
				if p.FSType == s.Expect.FSType && (s.Expect.Partition == nil || p.Index == *s.Expect.Partition) {
					found = true
				}
			}
			if !found {
				t.Fatalf("image info reports no partition with fs_type %q (partition %v)", s.Expect.FSType, s.Expect.Partition)
			}

			cmd := func(args ...string) []string {
				a := append([]string{"image"}, args...)
				a = append(a, "--case", c, ref)
				return append(a, part...)
			}
			// each step: the command, then the artifacts it added, counted and bounded
			stepRun := func(name string, args []string, bound rxBound, keep func(evidence.ManifestRecord, evidence.Recovery) bool, extra func(rxNew)) {
				before, err := manifest(c)
				if err != nil {
					t.Fatal(err)
				}
				rxStepRun(t, name, args...)
				after, err := manifest(c)
				if err != nil {
					t.Fatal(err)
				}
				n := rxDiff(before, after)
				got := n.count(keep)
				t.Logf("step %s: %d artifacts counted (classes: %s; methods: %s)", name, got,
					rxTally(n, func(r evidence.Recovery) string { return r.Class }),
					rxTally(n, func(r evidence.Recovery) string { return r.Method }))
				if err := bound.check(got); err != nil {
					t.Errorf("step %s: %v", name, err)
				}
				if extra != nil {
					extra(n)
				}
			}
			st := s.Expect.Steps
			if r := st.Recover; r != nil {
				stepRun("recover", cmd("recover", "--all"), r.rxBound,
					func(m evidence.ManifestRecord, _ evidence.Recovery) bool {
						return m.Source.Kind == evidence.KindRecover
					},
					func(n rxNew) {
						have := map[string]bool{}
						for _, m := range n {
							have[m.SHA256] = true
							if rv := rxRecovery(m); m.Source.Kind == evidence.KindRecover && len(r.Methods) > 0 && !slices.Contains(r.Methods, rv.Method) {
								t.Errorf("step recover: an artifact has method %q, which the expectation does not allow", rv.Method)
							}
						}
						for _, h := range r.SHA256 {
							if !have[h] {
								t.Errorf("step recover: no new artifact has the expected content hash %s", h)
							}
						}
					})
			}
			if u := st.UseJournal; u != nil {
				stepRun("use_journal", cmd("recover", "--all", "--use-journal", u.Mode), u.rxBound,
					func(_ evidence.ManifestRecord, r evidence.Recovery) bool { return r.Method == "ext4-journal-inode" }, nil)
			}
			if r := st.Rollforward; r != nil {
				stepRun("rollforward", cmd("recover", "--rollforward", "--all"), *r,
					func(_ evidence.ManifestRecord, r evidence.Recovery) bool {
						return r.Class == evidence.ClassPostCheckpointFile
					}, nil)
			}
			if j := st.Journal; j != nil {
				stepRun("journal", cmd("journal", "report"), *j,
					func(_ evidence.ManifestRecord, r evidence.Recovery) bool {
						return r.Class == evidence.ClassJournalReport
					}, nil)
			}
			if cv := st.Carve; cv != nil {
				stepRun("carve", cmd("carve"), cv.rxBound,
					func(m evidence.ManifestRecord, _ evidence.Recovery) bool { return m.Source.Kind == evidence.KindCarve },
					func(n rxNew) {
						types := map[string]int{}
						for _, m := range n {
							for _, b := range rxRecovery(m).Basis {
								if ty, ok := strings.CutPrefix(b, "sig="); ok {
									types[ty]++
								}
							}
						}
						for ty, want := range cv.TypesMin {
							if types[ty] < want {
								t.Errorf("step carve: type %q found %d times, below the expected %d", ty, types[ty], want)
							}
						}
					})
			}
			if sl := st.Slack; sl != nil {
				stepRun("slack", cmd("slack"), *sl,
					func(m evidence.ManifestRecord, _ evidence.Recovery) bool { return m.Source.Kind == evidence.KindSlack }, nil)
			}
			if code, _ := run(t, Deps{}, "case", "verify", "--case", c); code != 0 {
				t.Errorf("case verify: exit %d, want 0", code)
			}
		})
	}
}

func TestRealImagesRecoveryDatabases(t *testing.T) {
	sets := rxDBSets(t, rxRoot(t))
	if len(sets) == 0 {
		t.Skip("the layout holds no recovery-db set")
	}
	for i, s := range sets {
		t.Run(fmt.Sprintf("set-%d", i+1), func(t *testing.T) {
			if err := rxCheckSHA256(filepath.Join(s.Dir, s.Expect.DB), s.Expect.DBSHA256, false); err != nil {
				t.Fatal(err)
			}
			c := recordstest.NewCase(t)
			acq := "logical"
			if s.Expect.Platform == "ios" {
				acq = "ios.backup"
			}
			if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": acq}); err != nil {
				t.Fatal(err)
			}
			var dbID string
			set := filepath.Base(s.Dir)
			for k, name := range []string{s.Expect.DB, s.Expect.DB + "-wal", s.Expect.DB + "-journal"} {
				data, err := os.ReadFile(filepath.Join(s.Dir, name)) //nolint:gosec // an operator-supplied sample
				if err != nil {
					if os.IsNotExist(err) && k > 0 {
						continue
					}
					t.Fatal(rxPathless(err))
				}
				rec := pcapture(t, c, "files/"+fmt.Sprint(k), evidence.Source{Kind: "file", RemotePath: "/recovery-db/" + set + "/" + name}, string(data))
				if k == 0 {
					dbID = rec.ID
				}
			}
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("platform %s, secure_delete %v", s.Expect.Platform, *s.Expect.SecureDelete)
			code, out := run(t, Deps{}, "sqlite", "recover", "--case", dir, "--json", dbID)
			if code != 0 && code != 1 {
				t.Fatalf("sqlite recover: exit %d", code)
			}
			var rep sqliteReportJSON
			if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
				t.Fatalf("sqlite recover json (exit %d): %v", code, err)
			}
			t.Logf("sqlite recover: exit %d, written %d, freelist pages zeroed %d", code, rep.Counts.Written, rep.Counts.FreelistPagesZero)
			if code == 1 {
				t.Logf("sqlite recover exit 1: incomplete %v, stopped %q, not started %q", rep.Incomplete, rep.Stopped, rep.NotStarted)
			}
			ex := s.Expect
			if ex.Written != nil {
				if err := ex.Written.check(int(rep.Counts.Written)); err != nil {
					t.Errorf("written: %v", err)
				}
			}
			if ex.FreelistPagesZeroed != nil {
				if err := ex.FreelistPagesZeroed.check(int(rep.Counts.FreelistPagesZero)); err != nil {
					t.Errorf("freelist_pages_zeroed: %v", err)
				}
			}
			for method, b := range ex.Methods {
				rows := int64(0)
				for _, m := range rep.Counts.Methods {
					if m.Method == method {
						rows = m.Rows
					}
				}
				t.Logf("method %s: %d rows", method, rows)
				if err := b.check(int(rows)); err != nil {
					t.Errorf("method %s: %v", method, err)
				}
			}
			if code, _ := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
				t.Errorf("case verify: exit %d, want 0", code)
			}
		})
	}
}
