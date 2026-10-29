package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// slackFATImage is a FAT16 image whose three files leave non-uniform residue in their last cluster, plus a
// non-uniform volume tail past the end of the filesystem.
func slackFATImage() []byte {
	files := []fattest.File{
		{Path: "A.TXT", Data: bytes.Repeat([]byte{'a'}, 10)},
		{Path: "B.TXT", Data: bytes.Repeat([]byte{'b'}, 1000)},
	}
	img, built := fattest.BuildLayout(fattest.Options{Type: 16, SecPerClus: 2}, files)
	g := built.Geometry
	cs := int64(g.SecPerClus * g.SectorSize)
	for _, f := range files {
		e := built.Find(f.Path, false)
		off := g.ClusterOffset(e.Clusters[len(e.Clusters)-1]) + int64(len(f.Data))%cs
		for i := int64(0); i < 32; i++ {
			img[off+i] = byte(0xA0 + i%7)
		}
	}
	tail := make([]byte, 4096)
	for i := range tail {
		tail[i] = byte(i*7 + 1)
	}
	return append(img, tail...)
}

// A slack artifact whose bytes were altered fails case verify with exit 4 and a reproduce problem, and the
// verify.run audit entry says ok false; the untouched case verifies with exit 0.
func TestCaseVerifySlackTamperExits4(t *testing.T) {
	d := Deps{}
	dir := newCLICase(t)
	code, out := run(t, d, "image", "import", "--case", dir, "--json", imgFile(t, slackFATImage()))
	if code != 0 {
		t.Fatalf("image import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := s.ExportSlack(t.Context(), examine.SlackOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	var bin evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if strings.HasSuffix(a.Path, "/slack.bin") {
			bin = a
		}
	}
	if bin.ID == "" || sum.Files != 2 {
		t.Fatalf("setup: summary %+v", sum)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	code, out = run(t, d, "case", "verify", "--case", dir, "--json")
	if code != 0 {
		t.Fatalf("a genuine slack export must verify: exit %d\n%s", code, out)
	}
	var rep struct {
		Rechecked int      `json:"slack_rechecked"`
		Problems  []string `json:"problems"`
	}
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil || rep.Rechecked != 2 || len(rep.Problems) != 0 {
		t.Fatalf("slack_rechecked %d, problems %q (%v)", rep.Rechecked, rep.Problems, err)
	}
	if got := lastVerifyRun(t, dir)["slack_rechecked"]; got != float64(2) {
		t.Errorf("verify.run slack_rechecked = %v, want 2", got)
	}

	evidencetest.FlipArtifactByte(t, dir, bin, 0)
	code, out = run(t, d, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "PROBLEM: reproduce:") {
		t.Fatalf("exit %d, want 4 with a reproduce problem:\n%s", code, out)
	}
	if ok := lastVerifyRun(t, dir)["ok"]; ok != false {
		t.Errorf("verify.run ok = %v, want false", ok)
	}
}
