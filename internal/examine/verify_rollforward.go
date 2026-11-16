package examine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/volume"
)

// rollForwardMaxProblems is how many R12 problems are listed; the rest is one line.
const rollForwardMaxProblems = 50

// RollForwardCheck is R12 (3H-R13 e): every F2FS roll-forward artifact is recomputed from the image with the
// reader that wrote it, never trusted from its record. A report is rendered again (same limit, from its
// recorded max_bytes) and its SHA-256 and size compared; a block copy must be a node of the chain, with the
// recorded transaction, commit state and basis; a post-checkpoint file must be a post-checkpoint file of the
// chain and its recorded runs a prefix of the map the chain gives now. The bytes of block copies and files
// are reproduced by R6, and their runs lie in free space by R9. drivers are the filesystem drivers (nil =
// detect.Drivers). No option disables it. The reports it recomputed are passed to MarkRollForwardRan; one it
// could not recompute keeps the notice.
func RollForwardCheck(drivers []detect.Driver) evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		verifyRollForward(ctx, c, recs, rep, drivers)
	}
}

// rfRole is what R12 recomputes an artifact as.
type rfRole int

const (
	rfNone rfRole = iota
	rfReport
	rfBlock
	rfFile
)

func rfRoleOf(rec evidence.ManifestRecord) rfRole {
	d := rec.Source.Derived
	if d == nil || d.Recovery == nil {
		return rfNone
	}
	rv := d.Recovery
	rollForward := strings.HasPrefix(rv.Method, evidence.RollForwardMethod)
	switch {
	case rec.Source.Kind == evidence.KindReport && rv.Class == evidence.ClassJournalReport && rollForward:
		return rfReport
	case rec.Source.Kind == evidence.KindJournal && rv.Class == evidence.ClassJournalBlock && rollForward:
		return rfBlock
	case rec.Source.Kind == evidence.KindRecover && rv.Class == evidence.ClassPostCheckpointFile:
		return rfFile
	}
	return rfNone
}

type rfGroup struct {
	parent    string
	partition int
	offset    int64
	fsType    string
}

type rfChecker struct {
	ctx     context.Context
	c       *evidence.Case
	rep     *evidence.VerifyReport
	byID    map[string]evidence.ManifestRecord
	listed  int
	dropped int
	marked  []string
	add     func(n int64)
}

func (k *rfChecker) problem(rec evidence.ManifestRecord, format string, args ...any) {
	if k.listed >= rollForwardMaxProblems {
		k.dropped++
		return
	}
	k.listed++
	k.rep.AddProblem("rollforward: %s (artifact %q, %q)", fmt.Sprintf(format, args...), rec.ID, rec.Path)
}

func verifyRollForward(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport, drivers []detect.Driver) {
	k := &rfChecker{ctx: ctx, c: c, rep: rep, byID: make(map[string]evidence.ManifestRecord, len(recs)), add: func(int64) {}}
	groups := map[rfGroup][]evidence.ManifestRecord{}
	var total int64
	for _, rec := range recs {
		if _, dup := k.byID[rec.ID]; dup {
			continue // a duplicate id is reported once by the manifest checks
		}
		k.byID[rec.ID] = rec
		if rfRoleOf(rec) == rfNone {
			continue
		}
		d := rec.Source.Derived
		g := rfGroup{parent: d.ParentID, partition: d.Partition, offset: d.PartitionOffset, fsType: d.FSType}
		groups[g] = append(groups[g], rec)
		total++
	}
	done, add := c.VerifyPhase("rollforward", total, "artifacts")
	defer done()
	k.add = add
	keys := make([]rfGroup, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, func(x, y rfGroup) int {
		if r := strings.Compare(x.parent, y.parent); r != 0 {
			return r
		}
		if x.partition != y.partition {
			return x.partition - y.partition
		}
		if x.offset != y.offset {
			if x.offset < y.offset {
				return -1
			}
			return 1
		}
		return strings.Compare(x.fsType, y.fsType)
	})
	var (
		sess   *Session
		sessID string
		sessEr error
	)
	closeSess := func() {
		if sess != nil {
			_ = sess.Close()
			sess = nil
		}
	}
	defer closeSess()
	for _, g := range keys {
		if ctx.Err() != nil {
			break
		}
		members := groups[g]
		slices.SortFunc(members, func(x, y evidence.ManifestRecord) int {
			if r := strings.Compare(x.Path, y.Path); r != 0 {
				return r
			}
			return strings.Compare(x.ID, y.ID)
		})
		if sessID != g.parent || (sess == nil && sessEr == nil) {
			closeSess()
			sess, sessEr = OpenContainer(c, g.parent, Options{Drivers: drivers})
			if sessEr == nil {
				if perr := sess.ReadPartitions(); errors.Is(perr, evidence.ErrIntegrity) {
					sessEr = perr // an integrity failure is never read as an unreadable table
					closeSess()
				}
			}
			sessID = g.parent
		}
		k.group(sess, sessEr, g, members)
	}
	if k.dropped > 0 {
		rep.AddProblem("rollforward: %d further problems are not listed", k.dropped)
	}
	rep.MarkRollForwardRan(k.marked)
}

// group rechecks the artifacts of one partition.
func (k *rfChecker) group(s *Session, openErr error, g rfGroup, members []evidence.ManifestRecord) {
	skip := func(format string, args ...any) {
		k.problem(members[0], format+" (%d artifact(s) not rechecked)", append(args, len(members))...)
		k.add(int64(len(members)))
	}
	if openErr == nil && s.Table == nil {
		openErr = s.tableErr
	}
	if openErr != nil {
		skip("parent %q cannot be opened: %v", g.parent, openErr)
		return
	}
	fsys, part, err := s.FS(g.partition)
	if err != nil {
		skip("partition %d of parent %q cannot be opened: %v", g.partition, g.parent, err)
		return
	}
	if part.Start != g.offset {
		skip("partition %d starts at %d, the artifact records %d", g.partition, part.Start, g.offset)
		return
	}
	if t := fsys.Info().Type; t != g.fsType {
		skip("the filesystem is %q, the artifact records %q", t, g.fsType)
		return
	}
	rf, err := s.rollForwarderOf(fsys)
	if err != nil {
		skip("the filesystem of partition %d has no roll-forward analysis in this build", g.partition)
		return
	}
	nodes := map[uint64]rfBlockSel{}
	chain, err := rf.RollForward(func(n filesys.RollForwardNode) bool {
		nodes[n.Block] = rfBlockSel{index: n.Index, block: n.Block, nid: n.NID, ino: n.Ino, wouldReplay: n.WouldReplay}
		return true
	})
	if err != nil {
		skip("the roll-forward chain of partition %d cannot be read: %v", g.partition, err)
		return
	}
	ch := &rfChain{k: k, s: s, rf: rf, part: part, chain: chain, nodes: nodes}
	for _, rec := range members {
		if k.ctx.Err() != nil {
			return
		}
		ok := false
		switch rfRoleOf(rec) {
		case rfReport:
			ok = ch.report(rec)
		case rfBlock:
			ok = ch.block(rec)
		case rfFile:
			ok = ch.file(rec)
		}
		if ok {
			k.rep.RollForwardRechecked++
		}
		k.add(1)
	}
}

// rfChain is the freshly read chain of one partition.
type rfChain struct {
	k     *rfChecker
	s     *Session
	rf    filesys.RollForwarder
	part  volume.Partition
	chain filesys.RollForwardChain
	nodes map[uint64]rfBlockSel

	files    map[string]filesys.PostCheckpointFile
	filesErr error
	filesSet bool
}

// hashSink hashes what is written to it.
type hashSink struct {
	h hash.Hash
	n int64
}

func (w *hashSink) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return w.h.Write(p)
}

// report renders the report again and compares its SHA-256 and size with the recorded ones. It reports whether
// the artifact was recomputed and equal; a rendering that was made (equal or not) is marked as recomputed.
func (ch *rfChain) report(rec evidence.ManifestRecord) bool {
	k := ch.k
	d := rec.Source.Derived
	limit, err := strconv.ParseInt(d.Recovery.Params["max_bytes"], 10, 64)
	if err != nil || limit <= 0 {
		k.problem(rec, "report records no valid max_bytes parameter (%q), so it cannot be rendered again", d.Recovery.Params["max_bytes"])
		return false
	}
	sink := &hashSink{h: sha256.New()}
	chain, _, cut, err := renderRollForwardReport(sink, ch.part.Index, ch.rf, limit)
	if err != nil {
		k.problem(rec, "report cannot be rendered again: %v", err)
		return false
	}
	k.marked = append(k.marked, rec.ID)
	got := hex.EncodeToString(sink.h.Sum(nil))
	ok := true
	if got != rec.SHA256 || sink.n != rec.Size {
		k.problem(rec, "report differs from a fresh rendering (recorded %d bytes sha256 %s, rendered %d bytes sha256 %s)", rec.Size, rec.SHA256, sink.n, got)
		ok = false
	}
	if want := cut || !chain.Complete; rec.Incomplete != want {
		k.problem(rec, "report is recorded incomplete=%v, a fresh rendering is incomplete=%v", rec.Incomplete, want)
		ok = false
	}
	if j := d.Recovery.Journal; j == nil || j.Seq != uint32(chain.CheckpointVersion) || j.Committed != ch.anyReplay() || j.Region != "live" {
		k.problem(rec, "report's journal reference %+v differs from the chain (sequence %d, replayed %v)", j, uint32(chain.CheckpointVersion), ch.anyReplay())
		ok = false
	}
	return ok
}

func (ch *rfChain) anyReplay() bool {
	for _, n := range ch.nodes {
		if n.wouldReplay {
			return true
		}
	}
	return false
}

// block checks one block copy against the chain.
func (ch *rfChain) block(rec evidence.ManifestRecord) bool {
	k := ch.k
	d := rec.Source.Derived
	runs, err := k.c.DerivedRuns(rec, k.byID)
	if err != nil {
		k.problem(rec, "runs unreadable: %v", err)
		return false
	}
	if len(runs) != 1 || runs[0].Offset < 0 || runs[0].Length <= 0 || runs[0].Length > rfBlockSize {
		k.problem(rec, "block copy has %d runs (one run of at most %d bytes is expected)", len(runs), rfBlockSize)
		return false
	}
	rel := runs[0].Offset - ch.part.Start
	if rel < 0 || rel%rfBlockSize != 0 {
		k.problem(rec, "block copy at image offset %d is not at a block boundary of partition %d", runs[0].Offset, ch.part.Index)
		return false
	}
	blk := uint64(rel / rfBlockSize)
	n, ok := ch.nodes[blk]
	if !ok {
		k.problem(rec, "block at filesystem block %d is not a node of the chain", blk)
		return false
	}
	seq := uint32(ch.chain.CheckpointVersion)
	good := true
	if j := d.Recovery.Journal; j == nil || j.Seq != seq || j.Committed != n.wouldReplay || j.Region != "live" {
		k.problem(rec, "block %d's journal reference %+v differs from the chain (sequence %d, replayed %v)", blk, j, seq, n.wouldReplay)
		good = false
	}
	if want := rfBlockBasis(seq, n); !slices.Equal(d.Recovery.Basis, want) {
		k.problem(rec, "block %d's basis %v differs from the chain's %v", blk, d.Recovery.Basis, want)
		good = false
	}
	return good
}

// rfBlockBasis is the basis a block copy records.
func rfBlockBasis(seq uint32, n rfBlockSel) []string {
	return []string{
		fmt.Sprintf("rollforward:seq=%d", seq), fmt.Sprintf("chain-index=%d", n.index),
		fmt.Sprintf("nid=%d", n.nid), fmt.Sprintf("ino=%d", n.ino), fmt.Sprintf("blk=%d", n.block),
	}
}

// postCheckpoint lists the chain's post-checkpoint files once.
func (ch *rfChain) postCheckpoint() (map[string]filesys.PostCheckpointFile, error) {
	if !ch.filesSet {
		ch.filesSet = true
		files, err := ch.rf.PostCheckpointFiles()
		ch.filesErr = err
		ch.files = make(map[string]filesys.PostCheckpointFile, len(files))
		for _, f := range files {
			ch.files[f.ID] = f
		}
	}
	return ch.files, ch.filesErr
}

// file checks one recovered post-checkpoint file against the chain.
func (ch *rfChain) file(rec evidence.ManifestRecord) bool {
	k := ch.k
	d := rec.Source.Derived
	files, err := ch.postCheckpoint()
	if err != nil {
		k.problem(rec, "the post-checkpoint files of the chain cannot be listed: %v", err)
		return false
	}
	f, ok := files[d.FSID]
	if !ok {
		k.problem(rec, "%q is not a post-checkpoint file of the chain", d.FSID)
		return false
	}
	if f.Err != nil || len(f.Candidates) == 0 {
		k.problem(rec, "the chain gives no map for %q now (%v)", d.FSID, f.Err)
		return false
	}
	recorded, err := k.c.DerivedRuns(rec, k.byID)
	if err != nil {
		k.problem(rec, "runs unreadable: %v", err)
		return false
	}
	cand := f.Candidates[0]
	if d.Recovery == nil || d.Recovery.Method != cand.Method {
		got := ""
		if d.Recovery != nil {
			got = d.Recovery.Method
		}
		k.problem(rec, "recorded method %q differs from the method %q the chain gives now for %q", got, cand.Method, d.FSID)
		return false
	}
	switch {
	case rec.Size > cand.Size:
		k.problem(rec, "recorded size %d exceeds the %d bytes the chain gives now for %q", rec.Size, cand.Size, d.FSID)
		return false
	case rec.Size < cand.Size && !slices.Contains(d.Recovery.Assumptions, fmt.Sprintf("size-declared=%d", cand.Size)):
		k.problem(rec, "recorded size %d is short of the %d bytes the chain gives now for %q and does not declare it (size-declared=%d)", rec.Size, cand.Size, d.FSID, cand.Size)
		return false
	}
	fresh, ok := freshRuns(ch.part.Start, cand.Runs)
	if !ok {
		k.problem(rec, "the map the chain gives now for %q has a run whose image offset overflows", d.FSID)
		return false
	}
	if !runsArePrefix(recorded, fresh) {
		k.problem(rec, "recorded runs are not a prefix of the map the chain gives now for %q", d.FSID)
		return false
	}
	return true
}

// freshRuns is the filesystem-relative runs as image-relative ones; a hole stays a hole, and ok is false when
// an offset does not fit.
func freshRuns(partStart int64, runs []filesys.Run) ([]evidence.Run, bool) {
	fresh := make([]evidence.Run, len(runs))
	for i, r := range runs {
		fresh[i] = evidence.Run{Offset: r.Offset, Length: r.Length}
		if r.Offset >= 0 {
			off, ok := filesys.AddOK(partStart, r.Offset)
			if !ok {
				return nil, false
			}
			fresh[i].Offset = off
		}
	}
	return fresh, true
}

// runsArePrefix reports whether rec, read as bytes, is the first bytes of fresh: every run starts where the
// previous ended in the fresh map, and only the last may end inside a fresh run.
func runsArePrefix(rec, fresh []evidence.Run) bool {
	j := 0
	var used int64 // bytes of fresh[j] already matched
	for i, r := range rec {
		if r.Offset < 0 || r.Length <= 0 || j >= len(fresh) {
			return false
		}
		f := fresh[j]
		start, ok := filesys.AddOK(f.Offset, used)
		if !ok || r.Offset != start || r.Length > f.Length-used {
			return false
		}
		used += r.Length
		if used == f.Length {
			j++
			used = 0
		} else if i != len(rec)-1 {
			return false // a cut inside a run ends the prefix
		}
	}
	return true
}
