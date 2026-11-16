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
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// The kernel-written F2FS roll-forward crash image (internal/filesys/f2fs/testdata/rollforward/) through the real
// command and the real driver registry: journal report, block copies, recover --rollforward, then case verify (3H
// Task 14). The totals come from the independent oracle file; none is restated here.

type rfFixtureOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Files []struct {
		Path   string `json:"path"`
		Expect struct {
			Outcome string `json:"outcome"`
		} `json:"expect"`
	} `json:"files"`
}

func loadRollForwardFixture(t *testing.T) ([]byte, rfFixtureOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "f2fs", "testdata", "rollforward", "f2fs-rollforward")
	gz, err := os.ReadFile(base + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o rfFixtureOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatal("f2fs-rollforward: image sha256 does not match the oracle: regenerate image and oracle together")
	}
	return img, o
}

func TestImageRollForwardFixtureVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	img, o := loadRollForwardFixture(t)
	wantFiles := 0
	for _, f := range o.Files {
		if f.Expect.Outcome == "recover" {
			wantFiles++
		}
	}
	if wantFiles == 0 {
		t.Fatal("the oracle expects no recovered file")
	}
	c := newCLICase(t)
	d := Deps{} // the real driver registry
	ref := importRaw(t, c, gptWhole(img))

	code, out := run(t, d, "image", "journal", "ls", "--case", c, ref)
	m := txnLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("journal ls: %d\n%s", code, out)
	}
	seq := m[1]
	if code, out = run(t, d, "image", "journal", "report", "--case", c, ref); code != 0 || strings.Contains(out, "INCOMPLETE") {
		t.Fatalf("journal report: %d\n%s", code, out)
	}
	code, out = run(t, d, "image", "journal", "blocks", "--case", c, ref, seq)
	blocks := len(regexp.MustCompile(`journal/p1-f2fs/seq\d+-fsblk\d+\.bin`).FindAllString(out, -1))
	if code != 0 || blocks == 0 {
		t.Fatalf("journal blocks: %d, %d block copies\n%s", code, blocks, out)
	}
	if code, out = run(t, d, "image", "recover", "--case", c, ref, "--rollforward", "--all"); code != 0 {
		t.Fatalf("recover --rollforward: %d\n%s", code, out)
	}
	recs := recoveredRecords(t, c)
	if len(recs) < wantFiles {
		t.Fatalf("%d recovered artifacts, the oracle expects %d files", len(recs), wantFiles)
	}

	code, out = run(t, d, "case", "verify", "--case", c)
	if code != 0 {
		t.Fatalf("case verify: exit %d, want 0:\n%s", code, out)
	}
	if got := lastVerifyRun(t, c)["rollforward_rechecked"]; got == nil || got.(float64) < float64(1+blocks+wantFiles) {
		t.Errorf("rollforward_rechecked = %v, want at least %d (report, %d blocks, %d files)", got, 1+blocks+wantFiles, blocks, wantFiles)
	}

	// flip one byte of a recovered artifact: verify recomputes it and exits 4
	var victim bool
	for _, r := range recs {
		if r.Size > 0 {
			evidencetest.FlipArtifactByte(t, c, r, 0)
			victim = true
			break
		}
	}
	if !victim {
		t.Fatal("no non-empty recovered artifact to tamper with")
	}
	if code, out = run(t, d, "case", "verify", "--case", c); code != 4 || !strings.Contains(out, "PROBLEM:") {
		t.Errorf("case verify after the flipped byte: exit %d, want 4 with a PROBLEM:\n%s", code, out)
	}
}
