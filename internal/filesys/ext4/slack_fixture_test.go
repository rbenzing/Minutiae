package ext4_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// slackFixtureOracle is the part of testdata/slack/ext4-slack.expect.json the reader-level test needs. The oracle
// (tools/fixtures/slack_oracle.py) states what the filesystem's own tools and the raw image bytes say; it
// never runs Minutiae. The fixture is a kernel-written volume (QEMU guest), not byte-reproducible: the image
// hash is checked first.
type slackFixtureOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Unit      int64 `json:"unit"`
	FSSize    int64 `json:"fs_size"`
	DataStart int64 `json:"data_start"`
	Files     []struct {
		Path    string     `json:"path"`
		Size    int64      `json:"size"`
		Units   [][2]int64 `json:"units"` // inclusive unit numbers in the tools' numbering, merged when consecutive
		Outcome string     `json:"outcome"`
		Slack   *struct {
			FSOffset int64  `json:"fs_offset"`
			Length   int64  `json:"length"`
			SHA256   string `json:"sha256"`
		} `json:"slack"`
	} `json:"files"`
}

func loadSlackFixtureImage(t *testing.T) (slackFixtureOracle, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/slack/ext4-slack.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o slackFixtureOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	gz, err := os.ReadFile("testdata/slack/ext4-slack.img.gz")
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
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %x differs from the oracle's generator.image_sha256 %s: regenerate image and oracle together (gen.sh slack)", sum, o.Generator.ImageSHA256)
	}
	if len(o.Files) == 0 {
		t.Fatal("the oracle lists no files")
	}
	return o, img
}

// slackFixtureMerge joins runs that follow each other directly (a reader may report one run per extent).
func slackFixtureMerge(rs []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range rs {
		if k := len(out); k > 0 && out[k-1].Offset+out[k-1].Length == r.Offset {
			out[k-1].Length += r.Length
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestExt4AllocatedRunsMatchSlackOracle is a real-image oracle test: AllocatedRuns of every file of the
// kernel-written image equals the units the filesystem's tools report, and TailSlack gives the oracle's
// outcome and, for slack, its range.
func TestExt4AllocatedRunsMatchSlackOracle(t *testing.T) {
	o, img := loadSlackFixtureImage(t)
	fsys, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	info := fsys.Info()
	if int64(info.BlockSize) != o.Unit || info.Size != o.FSSize {
		t.Fatalf("Info: block size %d, size %d; oracle unit %d, fs_size %d", info.BlockSize, info.Size, o.Unit, o.FSSize)
	}
	if !filesys.ReportsSlack(fsys) {
		t.Fatal("ReportsSlack = false")
	}
	outcomes := map[string]int{}
	for _, w := range o.Files {
		e, err := fsys.Lookup(w.Path)
		if err != nil {
			t.Errorf("%s: Lookup: %v", w.Path, err)
			continue
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Errorf("%s: Open: %v", w.Path, err)
			continue
		}
		if f.Size() != w.Size {
			t.Errorf("%s: Size = %d, oracle %d", w.Path, f.Size(), w.Size)
		}
		var want []filesys.Run
		for _, u := range w.Units {
			want = append(want, filesys.Run{Offset: u[0] * o.Unit, Length: (u[1] - u[0] + 1) * o.Unit})
		}
		alloc, ok := filesys.AllocatedRunsOf(f)
		if !ok {
			t.Errorf("%s: no AllocatedRuns", w.Path)
			continue
		}
		if got := slackFixtureMerge(alloc); !slices.Equal(got, want) {
			t.Errorf("%s: AllocatedRuns = %v, oracle units give %v", w.Path, got, want)
		}
		run, outcome, reason := filesys.TailSlack(f.Size(), f.Runs(), alloc, int64(info.BlockSize), info.Size)
		if string(outcome) != w.Outcome {
			t.Errorf("%s: TailSlack outcome %q (%s), oracle %q", w.Path, outcome, reason, w.Outcome)
			continue
		}
		outcomes[w.Outcome]++
		if w.Outcome != "slack" {
			continue
		}
		if run.Offset != w.Slack.FSOffset || run.Length != w.Slack.Length {
			t.Errorf("%s: slack run %+v, oracle %d+%d", w.Path, run, w.Slack.FSOffset, w.Slack.Length)
			continue
		}
		sum := sha256.Sum256(img[run.Offset : run.Offset+run.Length])
		if hex.EncodeToString(sum[:]) != w.Slack.SHA256 {
			t.Errorf("%s: the image bytes of the slack run hash to %x, oracle %s", w.Path, sum, w.Slack.SHA256)
		}
	}
	if outcomes["slack"] == 0 || outcomes["exact-fit"] == 0 {
		t.Errorf("the scenario was not exercised: outcomes %v", outcomes)
	}
}
