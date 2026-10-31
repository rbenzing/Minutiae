package cli

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// cliSlackFixture reads the kernel-written fat16-slack image and the counts of its independent oracle
// (tools/fixtures/slack_oracle.py). The image is not byte-reproducible: its hash is checked first and the
// counts are read from the oracle, never hard-coded.
func cliSlackFixture(t *testing.T) (img []byte, nonuniform, files int) {
	t.Helper()
	base := filepath.Join("..", "filesys", "fat", "testdata", "slack", "fat16-slack")
	gz, err := os.ReadFile(base + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	if img, err = io.ReadAll(zr); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Generator struct {
			ImageSHA256 string `json:"image_sha256"`
		} `json:"generator"`
		Files  []json.RawMessage `json:"files"`
		Counts struct {
			NonuniformSlack int `json:"nonuniform_slack"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %x differs from the oracle's generator.image_sha256 %s: regenerate image and oracle together (tools/fixtures/gen.sh slack)", sum, o.Generator.ImageSHA256)
	}
	if o.Counts.NonuniformSlack == 0 {
		t.Fatal("the oracle counts no non-uniform slack")
	}
	return img, o.Counts.NonuniformSlack, len(o.Files)
}

type slackVerifyJSON struct {
	SlackRechecked int      `json:"slack_rechecked"`
	Problems       []string `json:"problems"`
}

func verifyViaCLI(t *testing.T, caseDir string) (code int, rep slackVerifyJSON, out string) {
	t.Helper()
	code, out = run(t, Deps{}, "case", "verify", "--case", caseDir, "--json")
	if code == 0 {
		if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
			t.Fatalf("verify json %q: %v", out, err)
		}
	}
	return code, rep, out
}

// Real-image oracle: image slack on the kernel-written FAT16 image, then case verify. The segment count
// equals the oracle's, case verify reproduces (R6) and re-derives (R11) both slack artifacts, and a flipped
// byte in an exported segment is exit 4.
func TestImageSlackFixtureRun(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports a real-image fixture")
	}
	img, nonuniform, files := cliSlackFixture(t)
	e := newSlackEnv(t, img)
	code, out, errOut := e.slack("--json")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	var j jsonSlack
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if j.FileSegments != nonuniform || j.FilesExamined != files || j.Incomplete || j.FSType != "fat16" {
		t.Errorf("file_segments %d (oracle %d), files_examined %d (oracle %d), incomplete %v, fs %q", j.FileSegments, nonuniform, j.FilesExamined, files, j.Incomplete, j.FSType)
	}
	code, rep, vout := verifyViaCLI(t, e.c)
	if code != 0 || rep.SlackRechecked != 2 || len(rep.Problems) != 0 {
		t.Fatalf("case verify: exit %d slack_rechecked %d problems %q\n%s", code, rep.SlackRechecked, rep.Problems, vout)
	}
	var bin evidence.ManifestRecord
	for _, a := range j.Artifacts {
		if strings.HasSuffix(a.Path, "/slack.bin") {
			bin = a
		}
	}
	if bin.ID == "" {
		t.Fatalf("no slack.bin among %d artifacts", len(j.Artifacts))
	}
	evidencetest.FlipArtifactByte(t, e.c, bin, bin.Size/2)
	code, out = run(t, Deps{}, "case", "verify", "--case", e.c)
	if code != 4 || !strings.Contains(out, "PROBLEM: reproduce:") {
		t.Errorf("a flipped byte in slack.bin: exit %d, want 4 with a reproduce problem\n%s", code, out)
	}
}

// 3F-T11 carry-over: a run cut by a directory that cannot be read to its end is incomplete work that is kept and
// flagged; case verify on that case is clean (exit 0), never an integrity failure.
func TestImageSlackCutRunVerifies(t *testing.T) {
	e := newSlackEnv(t, slackCutImage(t))
	if code, out, errOut := e.slack(); code != 0 || !strings.Contains(out, "INCOMPLETE: 1 directory not read to the end") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	code, rep, vout := verifyViaCLI(t, e.c)
	if code != 0 || len(rep.Problems) != 0 || rep.SlackRechecked == 0 {
		t.Fatalf("case verify of an incomplete slack run: exit %d slack_rechecked %d problems %q\n%s", code, rep.SlackRechecked, rep.Problems, vout)
	}
}

// 3F-T11 carry-over: the INCOMPLETE line of a run that is not a walk cut (a limit here; the same branch prints a
// plan that could not check a live file, 3F-C2, which no real FAT image can produce: a broken chain gives a
// prefix, and examine's TestPlanSlackUncheckedFileMakesPlanIncomplete drives that case with a fake
// filesystem). Exit 0, the artifact is flagged, the limit is named, and case verify is clean.
func TestImageSlackLimitPrintsIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports a real-image fixture")
	}
	img, _, _ := cliSlackFixture(t)
	e := newSlackEnv(t, img)
	code, out, errOut := e.slack("--files", "--max-bytes", "3000")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if strings.Contains(out, "directory not read") || !strings.Contains(out, "INCOMPLETE: ") || !strings.Contains(out, "limit reached: max-bytes") {
		t.Errorf("want an INCOMPLETE line that is not a walk cut and the named limit:\n%s", out)
	}
	var fileLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[recovered:slack-file") {
			fileLine = l
		}
	}
	if !strings.HasSuffix(fileLine, " [incomplete]") {
		t.Errorf("file-slack line = %q", fileLine)
	}
	_, jout, _ := e.slack("--files", "--json", "--max-bytes", "3000")
	var j jsonSlack
	if err := json.Unmarshal([]byte(jout), &j); err != nil || !j.Incomplete || j.WalkErrors != 0 || j.LimitReached != "max-bytes" {
		t.Errorf("json = incomplete %v walk_errors %d limit %q, %v", j.Incomplete, j.WalkErrors, j.LimitReached, err)
	}
	if code, rep, vout := verifyViaCLI(t, e.c); code != 0 || len(rep.Problems) != 0 {
		t.Fatalf("case verify: exit %d problems %q\n%s", code, rep.Problems, vout)
	}
}
