package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// jrnReport writes the report and returns its summary and artifact.
func jrnReport(t *testing.T, s *examine.Session, maxBytes int64) (examine.JournalReportSummary, evidence.ManifestRecord) {
	t.Helper()
	sum, err := s.JournalReport(context.Background(), jo, maxBytes)
	if err != nil {
		t.Fatalf("JournalReport: %v", err)
	}
	if len(sum.Artifacts) != 1 {
		t.Fatalf("%d artifacts, want 1", len(sum.Artifacts))
	}
	return sum, sum.Artifacts[0]
}

// jrnLines parses the lines of a report; each is a JSON object.
func jrnLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func jrnChain(t *testing.T, s *examine.Session) (filesys.RollForwardChain, []filesys.RollForwardNode) {
	t.Helper()
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	rf, ok := filesys.As[filesys.RollForwarder](fsys)
	if !ok {
		t.Fatal("no roll-forwarder")
	}
	var nodes []filesys.RollForwardNode
	chain, err := rf.RollForward(func(n filesys.RollForwardNode) bool { nodes = append(nodes, n); return true })
	if err != nil {
		t.Fatal(err)
	}
	return chain, nodes
}

func TestJournalReportWritesOneReport(t *testing.T) {
	s, _, _ := jrnSession(t)
	chain, nodes := jrnChain(t, s)
	sum, rec := jrnReport(t, s, 0)
	rv := recoveryOf(rec)
	d := rec.Source.Derived
	if rec.Source.Kind != evidence.KindReport || rv.Class != evidence.ClassJournalReport || rv.Method != "f2fs-rollforward" || rv.Confidence != nil {
		t.Errorf("kind %q recovery %+v", rec.Source.Kind, rv)
	}
	if len(d.Runs) != 0 || d.RunsArtifact != "" {
		t.Errorf("a report has no runs: %+v", d)
	}
	if !strings.HasSuffix(rec.Path, "reports/p1-f2fs/rollforward.jsonl") {
		t.Errorf("path %q", rec.Path)
	}
	if rv.Journal == nil || rv.Journal.Seq != uint32(chain.CheckpointVersion) || rv.Journal.Region != "live" {
		t.Errorf("journal ref %+v", rv.Journal)
	}
	if rec.Incomplete || sum.Files != 2 || sum.Summary.Files != 1 || sum.Nodes != chain.Nodes || sum.PostCheckpoint != 2 || !sum.Complete || sum.Path != rec.Path {
		t.Errorf("summary %+v, incomplete %v", sum, rec.Incomplete)
	}
	raw := readArtifact(t, s.Case, rec)
	if !bytes.HasPrefix(raw, []byte(`{"type":"chain","format":"minutiae-f2fs-rollforward/1","partition":1,"checkpoint_version":`)) {
		t.Errorf("chain line / field order: %.160s", raw)
	}
	lines := jrnLines(t, raw)
	var got []map[string]any
	files := 0
	for _, l := range lines {
		switch l["type"] {
		case "node":
			got = append(got, l)
		case "file":
			files++
		}
	}
	if len(got) != len(nodes) || files != 2 || lines[len(lines)-1]["type"] != "end" {
		t.Fatalf("%d node lines (want %d), %d file lines, last %v", len(got), len(nodes), files, lines[len(lines)-1])
	}
	for i, n := range nodes {
		g := got[i]
		if int(g["index"].(float64)) != n.Index || uint64(g["block"].(float64)) != n.Block || uint32(g["nid"].(float64)) != n.NID ||
			uint32(g["ino"].(float64)) != n.Ino || g["kind"] != n.Kind || g["would_replay"] != n.WouldReplay || g["live_ino"] != n.LiveIno {
			t.Errorf("node %d: line %v, node %+v", i, g, n)
		}
	}
	if end := lines[len(lines)-1]; int(end["nodes"].(float64)) != chain.Nodes || int(end["files"].(float64)) != 2 || int(end["post_checkpoint"].(float64)) != 2 {
		t.Errorf("end line %v", end)
	}
	if starts := auditByAction(t, s.Case, "analysis.start"); len(starts) != 1 {
		t.Errorf("%d analysis.start entries", len(starts))
	}
	verifyOK(t, s.Case)
}

func TestJournalReportDeterministic(t *testing.T) {
	var out [2][]byte
	for i := range out {
		s, _, _ := jrnSession(t)
		_, rec := jrnReport(t, s, 0)
		out[i] = readArtifact(t, s.Case, rec)
	}
	if !bytes.Equal(out[0], out[1]) || len(out[0]) == 0 {
		t.Errorf("two runs differ (%d and %d bytes)", len(out[0]), len(out[1]))
	}
}

// jrnExpand decodes address runs back to words.
func jrnExpand(runs []any) []uint32 {
	var words []uint32
	for _, r := range runs {
		p := r.([]any)
		first, count := uint32(p[0].(float64)), int(p[1].(float64))
		for i := range count {
			if first == 0 {
				words = append(words, 0)
			} else {
				words = append(words, first+uint32(i))
			}
		}
	}
	return words
}

func TestJournalReportAddressRunsLossless(t *testing.T) {
	o := rwOpts()
	const direct = 923 + 80
	var a, b, directBlk uint32
	var want []uint32
	s, _, _ := f2Session(t, o, []f2fstest.File{rwRF, {Path: "/rf/big.bin", Data: f2Stamp(direct*bs, "g"), PostCheckpoint: true}},
		func(img []byte, tr *f2fstest.Tree) {
			directBlk = tr.ChainOf["/rf/big.bin"][1]
			blk := f2Blk(img, directBlk)
			main := f2fstest.Geometry(f2Opts(o)).Main
			a, b = main+40, main+90
			for i := range 1018 {
				f2le.PutUint32(blk[4*i:], 0)
			}
			f2SetAddrs(blk, 0, a, a+1, a+2, 0, 0, b)
			for i := range 1018 {
				want = append(want, f2le.Uint32(blk[4*i:]))
			}
		})
	_, rec := jrnReport(t, s, 0)
	var node map[string]any
	for _, l := range jrnLines(t, readArtifact(t, s.Case, rec)) {
		if l["type"] == "node" && uint32(l["block"].(float64)) == directBlk {
			node = l
		}
	}
	if node == nil {
		t.Fatalf("no node line for block %d", directBlk)
	}
	wantRuns := fmt.Sprintf("[[%d,3],[0,2],[%d,1],[0,1012]]", a, b)
	if got, _ := json.Marshal(node["addrs"]); string(got) != wantRuns {
		t.Errorf("addrs = %s, want %s", got, wantRuns)
	}
	words := jrnExpand(node["addrs"].([]any))
	if len(words) != len(want) {
		t.Fatalf("expanded to %d words, want %d", len(words), len(want))
	}
	for i := range want {
		if words[i] != want[i] {
			t.Fatalf("word %d = %d, want %d", i, words[i], want[i])
		}
	}
}

func TestJournalReportIncompleteFlagged(t *testing.T) {
	var stop uint32
	s, _, _ := jrnSession(t, func(img []byte, tr *f2fstest.Tree) {
		stop = tr.ChainAddrs[len(tr.ChainAddrs)-1] + 1
		f2fstest.SetFooter(img, stop, f2fstest.Footer{NID: 20, Ino: 0, CPVer: f2fstest.ChainFooterVersion(img)})
	})
	sum, rec := jrnReport(t, s, 0)
	want := fmt.Sprintf("footer@%d", stop)
	if !rec.Incomplete || !strings.Contains(rec.Error, want) || sum.Complete || sum.Stop != want || sum.Files != 2 || sum.Summary.Files != 0 {
		t.Errorf("incomplete %v error %q summary %+v, want a report cut at %s", rec.Incomplete, rec.Error, sum, want)
	}
	if first := jrnLines(t, readArtifact(t, s.Case, rec))[0]; first["complete"] != false || first["stop"] != want {
		t.Errorf("chain line %v", first)
	}
	verifyOK(t, s.Case)
}

func TestJournalReportMaxBytesCut(t *testing.T) {
	s, _, _ := jrnSession(t)
	chain, _ := jrnChain(t, s)
	if _, err := s.JournalReport(context.Background(), jo, -1); !errors.Is(err, examine.ErrInvalidLimit) {
		t.Errorf("negative limit: %v", err)
	}
	if got := len(auditByAction(t, s.Case, "analysis.start")); got != 0 {
		t.Errorf("a refused limit audited %d analysis.start entries", got)
	}
	_, rec := jrnReport(t, s, 900)
	lines := jrnLines(t, readArtifact(t, s.Case, rec))
	last := lines[len(lines)-1]
	if last["type"] != "cut" || last["reason"] != "max-bytes" || !rec.Incomplete || !strings.Contains(rec.Error, "max-bytes") {
		t.Fatalf("last line %v, incomplete %v, error %q", last, rec.Incomplete, rec.Error)
	}
	if after := int(last["after_nodes"].(float64)); after >= chain.Nodes || after != len(lines)-2 {
		t.Errorf("after_nodes %d of %d nodes, %d lines", after, chain.Nodes, len(lines))
	}
	if rv := recoveryOf(rec); rv.Params["max_bytes"] != "900" {
		t.Errorf("params %v: the limit must be recorded so verify can render the same cut", rv.Params)
	}
	verifyOK(t, s.Case)
}
