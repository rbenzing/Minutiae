package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
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
	"github.com/rbenzing/minutiae/internal/examine"
)

// slackFixtures are the kernel-written, sentinel-filled images of plan 3F (tools/fixtures/slack.sh). Their
// oracle (tools/fixtures/slack_oracle.py) states, from the finished image alone, which files have slack, where it
// is and what it hashes to; it never runs Minutiae. The images are not byte-reproducible: every test checks the
// image hash first and reads every count and file from the oracle.
var slackFixtures = []struct{ fs, name string }{
	{"fat", "fat16-slack"},
	{"exfat", "exfat-slack"},
	{"ext4", "ext4-slack"},
	{"f2fs", "f2fs-slack"},
}

type slackOracleRange struct {
	ImageOffset int64  `json:"image_offset"`
	Length      int64  `json:"length"`
	SHA256      string `json:"sha256"`
	Uniform     bool   `json:"uniform"`
}

type slackOracleFile struct {
	Path    string            `json:"path"`
	Size    int64             `json:"size"`
	Outcome string            `json:"outcome"`
	Slack   *slackOracleRange `json:"slack"`
}

type slackOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type    string            `json:"type"`
	Unit    int64             `json:"unit"`
	Files   []slackOracleFile `json:"files"`
	Deleted []struct {
		Path                string `json:"path"`
		LastUnitImageOffset int64  `json:"last_unit_image_offset"`
		Unit                int64  `json:"unit"`
	} `json:"deleted"`
	Volume struct {
		Segments      []slackOracleRange `json:"segments"`
		UniformChunks int                `json:"uniform_chunks"`
		UniformBytes  int64              `json:"uniform_bytes"`
	} `json:"volume"`
	Counts struct {
		Slack           int `json:"slack"`
		UniformSlack    int `json:"uniform_slack"`
		NonuniformSlack int `json:"nonuniform_slack"`
		ExactFit        int `json:"exact-fit"`
		NoRuns          int `json:"no-runs"`
		TrailingHole    int `json:"trailing-hole"`
	} `json:"counts"`
}

// slackFiles returns the oracle files with the outcome and, when uniform is non-nil, the uniform flag.
func (o slackOracle) slackFiles(uniform *bool) (out []slackOracleFile) {
	for _, f := range o.Files {
		if f.Outcome != "slack" || f.Slack == nil || (uniform != nil && f.Slack.Uniform != *uniform) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func loadSlackFixture(t *testing.T, fs, name string) ([]byte, slackOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", fs, "testdata", "slack", name)
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
	var o slackOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s differs from the oracle's generator.image_sha256 %s: regenerate image and oracle together (tools/fixtures/gen.sh slack)", got, o.Generator.ImageSHA256)
	}
	if len(o.Files) == 0 || o.Unit == 0 || len(o.Volume.Segments) == 0 {
		t.Fatalf("the oracle is empty: %d files, unit %d, %d volume segments", len(o.Files), o.Unit, len(o.Volume.Segments))
	}
	return img, o
}

// exportSlackFixture imports the table-less image as a whole disk (so image offsets are the oracle's) and
// exports its slack; the run must be complete.
func exportSlackFixture(t *testing.T, img []byte, o examine.SlackOptions) (*examine.Session, examine.SlackSummary) {
	t.Helper()
	c := newCase(t)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // default driver registry
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	o.Partition = -1
	sum, err := s.ExportSlack(context.Background(), o)
	if err != nil {
		t.Fatalf("ExportSlack: %v", err)
	}
	if sum.Incomplete {
		t.Fatalf("the run is incomplete: %+v", sum)
	}
	return s, sum
}

// slackArtifact returns the record of the artifact whose path ends in name.
func slackArtifact(sum examine.SlackSummary, name string) (evidence.ManifestRecord, bool) {
	for _, a := range sum.Artifacts {
		if strings.HasSuffix(a.Path, "/"+name) {
			return a, true
		}
	}
	return evidence.ManifestRecord{}, false
}

// slackMapOf reads the exported lines of a sidecar with the strict reader.
func slackMapOf(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord, method string) []evidence.SlackMapLine {
	t.Helper()
	f, err := os.Open(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []evidence.SlackMapLine
	if err := evidence.ReadSlackRunMap(f, method, func(l evidence.SlackMapLine) error {
		if l.Exported {
			lines = append(lines, l)
		}
		return nil
	}); err != nil {
		t.Fatalf("%s: %v", rec.Path, err)
	}
	return lines
}

func sha256Of(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// requireFileLines compares the sidecar's lines with the oracle files as a set keyed by path: image offset,
// length, file size and uniform flag, and the artifact bytes at the line's offset against the oracle hash.
func requireFileLines(t *testing.T, lines []evidence.SlackMapLine, bin []byte, want []slackOracleFile) {
	t.Helper()
	got := map[string]evidence.SlackMapLine{}
	for _, l := range lines {
		if _, dup := got[l.FSPath]; dup {
			t.Errorf("%s: two lines", l.FSPath)
		}
		got[l.FSPath] = l
	}
	if len(got) != len(want) {
		t.Errorf("%d file segments, the oracle has %d", len(got), len(want))
	}
	for _, w := range want {
		l, ok := got[w.Path]
		if !ok {
			t.Errorf("%s: no segment, the oracle has one at image offset %d", w.Path, w.Slack.ImageOffset)
			continue
		}
		if l.ImageOffset != w.Slack.ImageOffset || l.Length != w.Slack.Length || l.FileSize != w.Size || l.Uniform != w.Slack.Uniform || l.PathEscaped {
			t.Errorf("%s: line {image %d len %d size %d uniform %v escaped %v}, oracle {image %d len %d size %d uniform %v}",
				w.Path, l.ImageOffset, l.Length, l.FileSize, l.Uniform, l.PathEscaped, w.Slack.ImageOffset, w.Slack.Length, w.Size, w.Slack.Uniform)
		}
		if l.Offset < 0 || l.Offset+l.Length > int64(len(bin)) {
			t.Errorf("%s: artifact range [%d,+%d) outside the %d artifact bytes", w.Path, l.Offset, l.Length, len(bin))
			continue
		}
		if sum := sha256Of(bin[l.Offset : l.Offset+l.Length]); sum != w.Slack.SHA256 {
			t.Errorf("%s: artifact bytes hash %s, oracle %s", w.Path, sum, w.Slack.SHA256)
		}
	}
}

// requireVolumeLines is the same for the volume segments, in order.
func requireVolumeLines(t *testing.T, lines []evidence.SlackMapLine, bin []byte, want []slackOracleRange) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("%d volume segments, the oracle has %d", len(lines), len(want))
	}
	for i, w := range want {
		l := lines[i]
		if l.ImageOffset != w.ImageOffset || l.Length != w.Length || l.Uniform != w.Uniform || l.FSPath != "" {
			t.Errorf("volume segment %d = {image %d len %d uniform %v}, oracle {image %d len %d uniform %v}", i, l.ImageOffset, l.Length, l.Uniform, w.ImageOffset, w.Length, w.Uniform)
		}
		if l.Offset < 0 || l.Offset+l.Length > int64(len(bin)) {
			t.Errorf("volume segment %d: artifact range outside the %d bytes", i, len(bin))
			continue
		}
		if sum := sha256Of(bin[l.Offset : l.Offset+l.Length]); sum != w.SHA256 {
			t.Errorf("volume segment %d: artifact bytes hash %s, oracle %s", i, sum, w.SHA256)
		}
	}
}

// requireVerifiesClean runs case verify with every composed check; R6 and R11 must have covered each .bin.
func requireVerifiesClean(t *testing.T, c *evidence.Case, sum examine.SlackSummary) {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
	n := 0
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindSlack {
			n++
		}
	}
	if rep.SlackRechecked != n || rep.RecoveredReproduced != n {
		t.Errorf("SlackRechecked %d, RecoveredReproduced %d, want %d (the slack artifacts) each", rep.SlackRechecked, rep.RecoveredReproduced, n)
	}
}

// requireOracleSkips: the three skip reasons the oracle counts equal its counts and no other reason is non-zero.
func requireOracleSkips(t *testing.T, sum examine.SlackSummary, o slackOracle) {
	t.Helper()
	want := map[string]int{"exact-fit": o.Counts.ExactFit, "no-runs": o.Counts.NoRuns, "trailing-hole": o.Counts.TrailingHole}
	for reason, n := range sum.SkippedBy {
		if n != want[reason] {
			t.Errorf("skipped %q = %d, the oracle says %d", reason, n, want[reason])
		}
	}
	for reason, n := range want {
		if sum.SkippedBy[reason] != n {
			t.Errorf("skipped %q = %d, the oracle says %d", reason, sum.SkippedBy[reason], n)
		}
	}
}

// Real-image oracle: the slack of four kernel-written images equals the independent expectation.
func TestSlackMatchesOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports real-image fixtures")
	}
	no, yes := false, true
	for _, fx := range slackFixtures {
		t.Run(fx.name, func(t *testing.T) {
			img, o := loadSlackFixture(t, fx.fs, fx.name)
			s, sum := exportSlackFixture(t, img, examine.SlackOptions{})
			if sum.FSType != o.Type || sum.Unit != o.Unit || sum.FilesExamined != len(o.Files) {
				t.Errorf("fs %q unit %d files %d; oracle %q %d %d", sum.FSType, sum.Unit, sum.FilesExamined, o.Type, o.Unit, len(o.Files))
			}
			// file slack: the non-uniform tails, as a set keyed by path
			wantFiles := o.slackFiles(&no)
			if len(wantFiles) != o.Counts.NonuniformSlack {
				t.Fatalf("the oracle lists %d non-uniform slack files but counts %d", len(wantFiles), o.Counts.NonuniformSlack)
			}
			var uniBytes int64
			uni := o.slackFiles(&yes)
			for _, f := range uni {
				uniBytes += f.Slack.Length
			}
			if len(uni) != o.Counts.UniformSlack || sum.UniformSegments != len(uni) || sum.UniformBytes != uniBytes {
				t.Errorf("uniform segments %d (%d bytes), oracle %d (%d bytes; its counts say %d)", sum.UniformSegments, sum.UniformBytes, len(uni), uniBytes, o.Counts.UniformSlack)
			}
			if sum.FileSegments != len(wantFiles) {
				t.Errorf("FileSegments %d, oracle %d", sum.FileSegments, len(wantFiles))
			}
			requireOracleSkips(t, sum, o)
			side, hasSide := slackArtifact(sum, "slack.runs.jsonl")
			bin, hasBin := slackArtifact(sum, "slack.bin")
			if hasSide != (len(wantFiles) > 0) || hasBin != hasSide {
				t.Errorf("file artifacts present: sidecar %v bin %v; the oracle has %d exportable tails", hasSide, hasBin, len(wantFiles))
			}
			if hasSide && hasBin {
				requireFileLines(t, slackMapOf(t, s.Case, side, evidence.MethodSlackFile), readArtifact(t, s.Case, bin), wantFiles)
			}
			// volume slack: the non-uniform segments
			var wantVol []slackOracleRange
			for _, g := range o.Volume.Segments {
				if !g.Uniform {
					wantVol = append(wantVol, g)
				}
			}
			if sum.VolumeUniformChunks != o.Volume.UniformChunks || sum.VolumeUniformBytes != o.Volume.UniformBytes {
				t.Errorf("volume uniform %d chunks %d bytes, oracle %d and %d", sum.VolumeUniformChunks, sum.VolumeUniformBytes, o.Volume.UniformChunks, o.Volume.UniformBytes)
			}
			vside, ok1 := slackArtifact(sum, "volume-slack.runs.jsonl")
			vbin, ok2 := slackArtifact(sum, "volume-slack.bin")
			if !ok1 || !ok2 {
				t.Fatalf("no volume artifacts: %+v", sum.Artifacts)
			}
			requireVolumeLines(t, slackMapOf(t, s.Case, vside, evidence.MethodSlackVolume), readArtifact(t, s.Case, vbin), wantVol)
			requireVerifiesClean(t, s.Case, sum)
		})
	}
}

// With IncludeZero every oracle tail (uniform included) and every volume segment is exported with the
// oracle's uniform flag.
func TestSlackIncludeZeroMatchesOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports real-image fixtures")
	}
	for _, fx := range slackFixtures {
		t.Run(fx.name, func(t *testing.T) {
			img, o := loadSlackFixture(t, fx.fs, fx.name)
			s, sum := exportSlackFixture(t, img, examine.SlackOptions{IncludeZero: true})
			wantFiles := o.slackFiles(nil)
			if len(wantFiles) != o.Counts.Slack {
				t.Fatalf("the oracle lists %d slack files but counts %d", len(wantFiles), o.Counts.Slack)
			}
			if sum.UniformSegments != 0 || sum.FileSegments != len(wantFiles) {
				t.Errorf("FileSegments %d UniformSegments %d, want %d and 0", sum.FileSegments, sum.UniformSegments, len(wantFiles))
			}
			requireOracleSkips(t, sum, o)
			side, ok1 := slackArtifact(sum, "slack.runs.jsonl")
			bin, ok2 := slackArtifact(sum, "slack.bin")
			if !ok1 || !ok2 {
				t.Fatalf("no file artifacts: %+v", sum.Artifacts)
			}
			requireFileLines(t, slackMapOf(t, s.Case, side, evidence.MethodSlackFile), readArtifact(t, s.Case, bin), wantFiles)
			vside, ok1 := slackArtifact(sum, "volume-slack.runs.jsonl")
			vbin, ok2 := slackArtifact(sum, "volume-slack.bin")
			if !ok1 || !ok2 {
				t.Fatalf("no volume artifacts: %+v", sum.Artifacts)
			}
			requireVolumeLines(t, slackMapOf(t, s.Case, vside, evidence.MethodSlackVolume), readArtifact(t, s.Case, vbin), o.Volume.Segments)
			if sum.VolumeUniformChunks != 0 {
				t.Errorf("VolumeUniformChunks %d with IncludeZero", sum.VolumeUniformChunks)
			}
			requireVerifiesClean(t, s.Case, sum)
		})
	}
}

// On ext4 and F2FS the kernel leaves nothing but zeros in a tail. The oracle says so (and the raw image is
// checked against it here), so a kernel that began to leave residue fails loudly; the default run then
// writes no file slack at all, only counts it.
func TestSlackSkipsUniform(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports real-image fixtures")
	}
	for _, fx := range slackFixtures[2:] {
		t.Run(fx.name, func(t *testing.T) {
			img, o := loadSlackFixture(t, fx.fs, fx.name)
			if o.Counts.Slack == 0 || o.Counts.NonuniformSlack != 0 || o.Counts.UniformSlack != o.Counts.Slack {
				t.Fatalf("the oracle counts %+v: expected only uniform tails", o.Counts)
			}
			for _, f := range o.slackFiles(nil) {
				g := f.Slack
				if !g.Uniform {
					t.Fatalf("%s: the oracle says the tail holds residue", f.Path)
				}
				if !bytes.Equal(img[g.ImageOffset:g.ImageOffset+g.Length], make([]byte, g.Length)) {
					t.Fatalf("%s: the raw image holds non-zero bytes at the oracle's tail", f.Path)
				}
			}
			s, sum := exportSlackFixture(t, img, examine.SlackOptions{})
			if _, ok := slackArtifact(sum, "slack.bin"); ok {
				t.Error("slack.bin written although every tail is uniform")
			}
			if _, ok := slackArtifact(sum, "slack.runs.jsonl"); ok {
				t.Error("slack.runs.jsonl written although every tail is uniform")
			}
			if sum.FileSegments != 0 || sum.FileBytes != 0 || sum.UniformSegments != o.Counts.UniformSlack {
				t.Errorf("FileSegments %d FileBytes %d UniformSegments %d, oracle uniform %d", sum.FileSegments, sum.FileBytes, sum.UniformSegments, o.Counts.UniformSlack)
			}
			requireVerifiesClean(t, s.Case, sum)
		})
	}
}

func uniformBytes(b []byte) bool {
	for _, c := range b {
		if c != b[0] {
			return false
		}
	}
	return true
}

// The freed unit of a deleted file still holds its residue, but slack is the tail of LIVE files: no
// exported file or volume segment, with or without --include-zero, overlaps that unit.
func TestSlackExcludesDeletedFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports real-image fixtures")
	}
	for _, fx := range slackFixtures {
		t.Run(fx.name, func(t *testing.T) {
			img, o := loadSlackFixture(t, fx.fs, fx.name)
			if len(o.Deleted) == 0 {
				t.Fatal("the oracle records no deleted file")
			}
			for _, d := range o.Deleted {
				if (fx.fs == "fat" || fx.fs == "exfat") && uniformBytes(img[d.LastUnitImageOffset:d.LastUnitImageOffset+d.Unit]) {
					// the precondition that makes the exclusion meaningful
					t.Fatalf("%s: the freed unit at %d is uniform; the test would pass vacuously", d.Path, d.LastUnitImageOffset)
				}
			}
			for _, includeZero := range []bool{false, true} {
				s, sum := exportSlackFixture(t, img, examine.SlackOptions{IncludeZero: includeZero})
				segments := 0
				for _, name := range []string{"slack.runs.jsonl", "volume-slack.runs.jsonl"} {
					rec, ok := slackArtifact(sum, name)
					if !ok {
						continue
					}
					method := evidence.MethodSlackFile
					if strings.HasPrefix(name, "volume") {
						method = evidence.MethodSlackVolume
					}
					for _, l := range slackMapOf(t, s.Case, rec, method) {
						segments++
						for _, d := range o.Deleted {
							if l.ImageOffset < d.LastUnitImageOffset+d.Unit && d.LastUnitImageOffset < l.ImageOffset+l.Length {
								t.Errorf("include-zero=%v: %s segment [%d,+%d) %q overlaps the freed unit of %s at [%d,+%d)",
									includeZero, name, l.ImageOffset, l.Length, l.FSPath, d.Path, d.LastUnitImageOffset, d.Unit)
							}
						}
					}
				}
				if segments == 0 {
					t.Errorf("include-zero=%v: nothing exported, so nothing was compared", includeZero)
				}
				requireVerifiesClean(t, s.Case, sum)
			}
		})
	}
}

// A flipped byte in an exported segment of a real-image export is a verify problem (internal/cli pins exit 4).
func TestSlackFixtureTamperIsAProblem(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and exports a real-image fixture")
	}
	img, _ := loadSlackFixture(t, "fat", "fat16-slack")
	s, sum := exportSlackFixture(t, img, examine.SlackOptions{})
	bin, ok := slackArtifact(sum, "slack.bin")
	if !ok {
		t.Fatal("no slack.bin")
	}
	evidencetest.FlipArtifactByte(t, s.Case.Dir, bin, bin.Size/2)
	rep, err := s.Case.VerifyWith(context.Background(), examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range rep.Problems {
		found = found || strings.Contains(p, "reproduce:")
	}
	if !found {
		t.Errorf("no reproduce problem for a flipped byte: %v", rep.Problems)
	}
}
