package examine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Recovery of post-checkpoint files (3H Task 9) through the real F2FS reader, on builder images whose
// warm-node log holds fsynced files that no checkpoint knows.

func rwOpts() f2fstest.Options {
	return f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery, ExtraAttr: true, InodeChksum: true}
}

// rwAddrAt is the byte offset of i_addr[0] in an inode block of a volume built with o.
func rwAddrAt(o f2fstest.Options) int {
	if o.ExtraAttr {
		return f2oAddr + 36
	}
	return f2oAddr
}

// rwSetAddr sets data address slot i of the inode block at addr and re-seals its checksum.
func rwSetAddr(o f2fstest.Options, img []byte, addr uint32, i int, v uint32) {
	f2le.PutUint32(f2Blk(img, addr)[rwAddrAt(o)+4*i:], v)
	f2fstest.ResealInode(f2Opts(o), img, addr)
}

func rwRun(t *testing.T, s *examine.Session, o examine.RecoverOptions) examine.RecoverSummary {
	t.Helper()
	o.Partition, o.RollForward = -1, true
	sum, err := s.Recover(context.Background(), o)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return sum
}

func rwAll(t *testing.T, s *examine.Session) examine.RecoverSummary {
	t.Helper()
	return rwRun(t, s, examine.RecoverOptions{All: true})
}

func rwRecords(sum examine.RecoverSummary) []evidence.ManifestRecord {
	var out []evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.Recovery != nil {
			out = append(out, a)
		}
	}
	return out
}

func rwPlan(t *testing.T, s *examine.Session, o examine.RecoverOptions) *examine.RecoverPlan {
	t.Helper()
	o.Partition, o.RollForward = -1, true
	plan, err := s.PlanRecovery(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func rwID(tr *f2fstest.Tree, p string) string { return fmt.Sprintf("rollforward:%d", tr.NID[p]) }

var rwRF = f2fstest.File{Path: "/rf", Dir: true}

func TestRecoverRollForwardSmallFile(t *testing.T) {
	data := f2Stamp(3*bs, "a")
	s, disk, tr := f2Session(t, rwOpts(), []f2fstest.File{rwRF, {Path: "/rf/a.bin", Data: data, PostCheckpoint: true}})
	sum := rwAll(t, s)
	rec := only(t, rwRecords(sum))
	rv := recoveryOf(rec)
	d := rec.Source.Derived
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) || rec.Incomplete {
		t.Fatalf("artifact differs from the source or is incomplete: %+v", rec)
	}
	if rv.Class != evidence.ClassPostCheckpointFile || rv.Method != "f2fs-rollforward" || *rv.Confidence != 60 ||
		evidence.ConfidenceBand(*rv.Confidence) != "medium" {
		t.Errorf("recovery %+v, confidence %d", rv, *rv.Confidence)
	}
	if d.FSPath != "/rf/a.bin" || d.FSID != rwID(tr, "/rf/a.bin") {
		t.Errorf("FSPath %q FSID %q", d.FSPath, d.FSID)
	}
	if !hasAssumption(rv, "not-checkpointed") || !hasAssumption(rv, "replay-not-applied") || hasAssumption(rv, "parent-path-unknown") {
		t.Errorf("assumptions %v", rv.Assumptions)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A file that needs an indirect node is cut there: the prefix, flagged incomplete, capped at 20 (C3).
func TestRecoverRollForwardIndirectIsPrefixCapped(t *testing.T) {
	o := rwOpts()
	o.ExtraAttr, o.InodeChksum = false, false
	const blocks = 923 + 2*1018
	data := f2Stamp(blocks*bs+5*bs, "i")
	s, disk, _ := f2Session(t, o, []f2fstest.File{{Path: "/ind", Data: data, PostCheckpoint: true}})
	sum := rwAll(t, s)
	rec := only(t, rwRecords(sum))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:blocks*bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), blocks*bs, rec.Incomplete, *rv.Confidence)
	}
	if !strings.Contains(rec.Error, "node-missing") || !hasAssumptionPrefix(rv, "stale-chain-stop=node-missing@") {
		t.Errorf("error %q, assumptions %v: node-missing is not named", rec.Error, rv.Assumptions)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A data block that a live file holds in the SIT cuts the map: the free prefix only, no live byte.
func TestRecoverRollForwardReusedBlockKeepsFreePrefix(t *testing.T) {
	o := rwOpts()
	data, live := f2Stamp(4*bs, "g"), f2Stamp(2*bs, "L")
	s, disk, _ := f2Session(t, o, []f2fstest.File{rwRF, {Path: "/live.bin", Data: live}, {Path: "/rf/a.bin", Data: data, PostCheckpoint: true}},
		func(img []byte, tr *f2fstest.Tree) {
			rwSetAddr(o, img, tr.ChainOf["/rf/a.bin"][0], 2, tr.Data["/live.bin"][0])
		})
	sum := rwAll(t, s)
	rec := only(t, rwRecords(sum))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:2*bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*bs, rec.Incomplete, *rv.Confidence)
	}
	// The unclean-checkpoint warning makes a non-free byte "unknown" rather than "allocated" (the 3D rule).
	if !hasAssumption(rv, "cut-state=unknown") || !hasAssumption(rv, "prefix-only") || bytes.Contains(got, live[:32]) {
		t.Errorf("assumptions %v, or a live byte in the artifact", rv.Assumptions)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverRollForwardOverlap(t *testing.T) {
	o := rwOpts()
	s, disk, tr := f2Session(t, o, []f2fstest.File{
		rwRF,
		{Path: "/rf/a.bin", Data: f2Stamp(3*bs, "a"), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: f2Stamp(3*bs, "b"), PostCheckpoint: true},
	}, func(img []byte, tr *f2fstest.Tree) {
		rwSetAddr(o, img, tr.ChainOf["/rf/b.bin"][0], 1, tr.Data["/rf/a.bin"][1])
	})
	sum := rwAll(t, s)
	recs := rwRecords(sum)
	if len(recs) != 2 || sum.Overlap != 2 {
		t.Fatalf("%d artifacts, overlap %d; want both written and flagged", len(recs), sum.Overlap)
	}
	for _, rec := range recs {
		rv := recoveryOf(rec)
		other := rwID(tr, "/rf/a.bin")
		if rec.Source.Derived.FSID == other {
			other = rwID(tr, "/rf/b.bin")
		}
		if *rv.Confidence != 30 || !hasAssumption(rv, "overlap="+other) {
			t.Errorf("%s: confidence %d, assumptions %v; want 30 and overlap=%s", rec.Source.Derived.FSID, *rv.Confidence, rv.Assumptions, other)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	verifyOK(t, s.Case)
}

// A chain that stops with later nodes unknown caps every file at 20, even a complete map (3H-R8).
func TestRecoverRollForwardChainCutCaps20(t *testing.T) {
	data := f2Stamp(3*bs, "c")
	var stopBlk uint32
	s, disk, _ := f2Session(t, rwOpts(), []f2fstest.File{rwRF, {Path: "/rf/a.bin", Data: data, PostCheckpoint: true}},
		func(img []byte, tr *f2fstest.Tree) {
			stopBlk = tr.ChainAddrs[len(tr.ChainAddrs)-1] + 1
			// A block of the right version whose footer names inode 0: the walk stops there, later nodes unknown.
			f2fstest.SetFooter(img, stopBlk, f2fstest.Footer{NID: 20, Ino: 0, CPVer: f2fstest.ChainFooterVersion(img)})
		})
	sum := rwAll(t, s)
	rec := only(t, rwRecords(sum))
	rv := recoveryOf(rec)
	want := fmt.Sprintf("rollforward-chain-cut=footer@%d", stopBlk)
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) || rec.Incomplete || *rv.Confidence != 20 || !hasAssumption(rv, want) {
		t.Fatalf("incomplete %v, confidence %d, assumptions %v; want the full map at 20 with %s", rec.Incomplete, *rv.Confidence, rv.Assumptions, want)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverRollForwardSkips(t *testing.T) {
	o := rwOpts()
	a := f2fstest.Geometry(f2Opts(o)).Main + 40
	node := func(nid uint32, in f2fstest.Inode) f2fstest.ChainNode {
		in.NID, in.Mode, in.Links, in.Extra = nid, 0o100644, 1, true
		if in.PIno == 0 {
			in.PIno = f2fstest.RootIno
		}
		if in.Name == "" {
			in.Name = fmt.Sprintf("f%d", nid)
		}
		return f2fstest.ChainNode{Block: f2fstest.InodeBlock(o, in), Fsync: true, Dentry: true}
	}
	oo := o
	oo.Chain = []f2fstest.ChainNode{
		node(91, f2fstest.Inode{InlineData: []byte("hi"), Size: 2}),
		node(92, f2fstest.Inode{PIno: 4000, Size: bs, Addrs: []uint32{a}}),
		node(93, f2fstest.Inode{Size: bs, Addrs: []uint32{a + 1}}),
		node(94, f2fstest.Inode{}),
	}
	s, _, _ := f2Session(t, oo, []f2fstest.File{{Path: "/", Dir: true}}, func(img []byte, tr *f2fstest.Tree) {
		img[int(tr.ChainAddrs[2])*bs+4] ^= 0xff // under the inode checksum
	})
	plan := rwPlan(t, s, examine.RecoverOptions{All: true})
	got := map[string]string{}
	for _, it := range plan.Items {
		for _, sk := range it.Skips {
			got[it.ID] = sk.Reason
		}
	}
	want := map[string]string{"rollforward:91": "unsupported", "rollforward:92": "unsupported", "rollforward:93": "corrupt", "rollforward:94": "empty"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skips %v, want %v", got, want)
	}
	sum := rwAll(t, s)
	if len(rwRecords(sum)) != 0 || sum.SkippedBy["unsupported"] != 2 || sum.SkippedBy["corrupt"] != 1 || sum.SkippedBy["empty"] != 1 ||
		sum.SkippedBy["undecided"] != 0 || len(sum.SkippedBy) != 3 {
		t.Errorf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(rwRecords(sum)))
	}
	verifyOK(t, s.Case)
}

// The parent is a directory inode the NAT still maps but no live dentry names: the file is a candidate, its
// path is unknown and says so.
func TestRecoverRollForwardParentPathUnknown(t *testing.T) {
	data := f2Stamp(2*bs, "p")
	s, disk, tr := f2Session(t, rwOpts(), []f2fstest.File{{Path: "/orphan", Dir: true, Deleted: true}, {Path: "/orphan/a.bin", Data: data, PostCheckpoint: true}})
	sum := rwAll(t, s)
	rec := only(t, rwRecords(sum))
	rv := recoveryOf(rec)
	if rec.Source.Derived.FSPath != "(post-checkpoint)/a.bin" || rec.Source.Derived.FSID != rwID(tr, "/orphan/a.bin") || !hasAssumption(rv, "parent-path-unknown") {
		t.Errorf("FSPath %q FSID %q assumptions %v", rec.Source.Derived.FSPath, rec.Source.Derived.FSID, rv.Assumptions)
	}
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) {
		t.Error("bytes differ from the source")
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverRollForwardRefs(t *testing.T) {
	s, _, tr := f2Session(t, rwOpts(), []f2fstest.File{
		rwRF,
		{Path: "/rf/a.bin", Data: f2Stamp(2*bs, "a"), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: f2Stamp(2*bs, "b"), PostCheckpoint: true},
	})
	sum := rwRun(t, s, examine.RecoverOptions{Refs: []string{"id:" + rwID(tr, "/rf/b.bin")}})
	if rec := only(t, rwRecords(sum)); rec.Source.Derived.FSID != rwID(tr, "/rf/b.bin") {
		t.Errorf("selected %q", rec.Source.Derived.FSID)
	}
	starts := len(auditByAction(t, s.Case, "analysis.start"))
	n := len(auditEntries(t, s.Case))
	for _, ref := range []string{"id:rollforward:999999", "id:nid:" + fmt.Sprint(tr.NID["/rf/a.bin"]), "/rf/a.bin", "id:"} {
		_, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, RollForward: true, Refs: []string{ref}})
		if err == nil {
			t.Fatalf("%s: no error", ref)
		}
		if ref == "id:rollforward:999999" && !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", ref, err)
		}
		if ref != "id:rollforward:999999" && errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%s: %v is ErrNotFound, want a usage error", ref, err)
		}
	}
	if len(auditByAction(t, s.Case, "analysis.start")) != starts || len(auditEntries(t, s.Case)) != n {
		t.Error("a refused reference wrote audit entries")
	}
	verifyOK(t, s.Case)
}

// Deleted entries and post-checkpoint files are never mixed in one run.
func TestRecoverRollForwardNeverDeletedFiles(t *testing.T) {
	files := []f2fstest.File{
		rwRF,
		{Path: "/d/gone.bin", Data: f2Stamp(3*bs, "g"), Deleted: true, Unlinked: true},
		{Path: "/rf/a.bin", Data: f2Stamp(2*bs, "a"), PostCheckpoint: true},
	}
	s, _, _ := f2Session(t, rwOpts(), files)
	rf := rwRecords(rwAll(t, s))
	if len(rf) != 1 || recoveryOf(rf[0]).Class != evidence.ClassPostCheckpointFile {
		t.Fatalf("roll-forward run wrote %d artifacts: %+v", len(rf), rf)
	}
	normal := rwRecords(recoverAll(t, s))
	if len(normal) != 1 || recoveryOf(normal[0]).Class != evidence.ClassDeletedFile || normal[0].Source.Derived.FSPath != "/d/gone.bin" {
		t.Fatalf("normal run wrote %d artifacts: %+v", len(normal), normal)
	}
	verifyOK(t, s.Case)
}

func TestRecoverRollForwardNoRollForwarder(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs)))
	n := len(auditEntries(t, e.c))
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, RollForward: true})
	if !errors.Is(err, examine.ErrNoRollForward) || !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrNoRollForward", err)
	}
	if got := len(auditEntries(t, e.c)); got != n || len(auditByAction(t, e.c, "analysis.start")) != 0 {
		t.Errorf("audit grew by %d, analysis.start written", got-n)
	}
	if _, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, RollForward: true}); !errors.Is(err, examine.ErrNoRollForward) {
		t.Errorf("PlanRecovery err = %v", err)
	}
}

func TestRecoverRollForwardListMatchesRun(t *testing.T) {
	o := rwOpts()
	files := []f2fstest.File{
		rwRF,
		{Path: "/live.bin", Data: f2Stamp(bs, "L")},
		{Path: "/rf/ok.bin", Data: f2Stamp(3*bs, "a"), PostCheckpoint: true},
		{Path: "/rf/cut.bin", Data: f2Stamp(4*bs, "b"), PostCheckpoint: true},
		{Path: "/rf/sparse.bin", Data: f2Stamp(5*bs, "c"), PostCheckpoint: true},
	}
	s, _, _ := f2Session(t, o, files, func(img []byte, tr *f2fstest.Tree) {
		rwSetAddr(o, img, tr.ChainOf["/rf/cut.bin"][0], 1, tr.Data["/live.bin"][0])
		rwSetAddr(o, img, tr.ChainOf["/rf/sparse.bin"][0], 3, 0)
	})
	type shape struct {
		Method     string
		Confidence int
		Runs       []evidence.Run
		Size       int64
	}
	plan := rwPlan(t, s, examine.RecoverOptions{All: true})
	planned := map[string]shape{}
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned[it.ID] = shape{c.Method, c.Confidence, c.Runs, c.Size}
			}
		}
	}
	written := map[string]shape{}
	for _, a := range rwRecords(rwAll(t, s)) {
		rv := recoveryOf(a)
		written[a.Source.Derived.FSID] = shape{rv.Method, *rv.Confidence, a.Source.Derived.Runs, a.Size}
	}
	if len(planned) != 3 || !reflect.DeepEqual(planned, written) {
		t.Fatalf("planned %+v\nwritten %+v", planned, written)
	}
}

func TestRecoverRollForwardNeverModifiesSource(t *testing.T) {
	s, disk, _ := f2Session(t, rwOpts(), []f2fstest.File{rwRF, {Path: "/rf/a.bin", Data: f2Stamp(2*bs, "a"), PostCheckpoint: true}})
	before := sha256hex(readArtifact(t, s.Case, s.Parent))
	if before != sha256hex(disk) {
		t.Fatal("the imported artifact differs from the source before the run")
	}
	rwAll(t, s)
	if after := sha256hex(readArtifact(t, s.Case, s.Parent)); after != before {
		t.Errorf("the image artifact changed: %s -> %s", before, after)
	}
	verifyOK(t, s.Case)
}

func TestRecoverRollForwardAuditDetails(t *testing.T) {
	s, _, _ := f2Session(t, rwOpts(), []f2fstest.File{rwRF, {Path: "/rf/a.bin", Data: f2Stamp(2*bs, "a"), PostCheckpoint: true}})
	rwAll(t, s)
	start, end := auditByAction(t, s.Case, "analysis.start"), auditByAction(t, s.Case, "analysis.end")
	if len(start) != 1 || len(end) != 1 {
		t.Fatalf("%d analysis.start, %d analysis.end", len(start), len(end))
	}
	if v, _ := start[0].Details["rollforward"].(bool); !v {
		t.Errorf("analysis.start details %v lack rollforward: true", start[0].Details)
	}
	d := end[0].Details
	if fmt.Sprint(d["chain_nodes"]) != "1" {
		t.Errorf("chain_nodes = %v, want 1", d["chain_nodes"])
	}
	if stop, ok := d["chain_stop"].(string); !ok || stop == "" {
		t.Errorf("chain_stop = %v", d["chain_stop"])
	}
	if c, ok := d["chain_complete"].(bool); !ok || !c {
		t.Errorf("chain_complete = %v, want true", d["chain_complete"])
	}
	// A deleted-entry run keeps its audit shape: no roll-forward keys.
	recoverAll(t, s)
	if _, ok := auditByAction(t, s.Case, "analysis.start")[1].Details["rollforward"]; ok {
		t.Error("a normal run's analysis.start carries rollforward")
	}
}
