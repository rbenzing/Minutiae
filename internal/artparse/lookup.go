package artparse

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// ErrLookupOpens is returned by Lookuper.Open past Limits.MaxLookupOpens.
var ErrLookupOpens = errors.New("artparse: too many Lookuper opens")

// lookuper is the parse.Lookuper of one Input. Every open goes through the
// bundle, which verifies it against the manifest, bounds it and seals it with
// the bundle.
type lookuper struct {
	b     *bundle
	probe *parse.ReadBudget
	own   *sealSet // the seal set of the Probe input this Lookuper belongs to; nil for a Parse input
}

// sealed reports whether the bundle, or the Probe input this Lookuper was made for, is sealed.
func (l *lookuper) sealed() bool {
	return l.b.seals.isSealed() || (l.own != nil && l.own.isSealed())
}

var _ parse.Lookuper = (*lookuper)(nil)

// lookupEligible: live artifacts only (the F6 allowlist without snapshots, and nothing with a recovered
// ancestor), plus the artifacts of the job's own snapshot when the job is snapshot-derived.
func (b *bundle) lookupEligible(m evidence.ManifestRecord) bool {
	if m.Source.Kind == "extract" {
		if _, recovered := b.snap.recoveryTrail(m.ID); recovered {
			return false
		}
	}
	if ok, _ := Eligible(m, false, false); ok {
		return true
	}
	own := b.job.Primary.Snapshot
	if own == nil {
		return false
	}
	if ok, _ := Eligible(m, true, false); !ok {
		return false
	}
	d := m.Source.Derived
	return d != nil && d.Snapshot != nil && snapshotKey(&parse.SnapshotInfo{Name: d.Snapshot.Name, Xid: d.Snapshot.Xid}) == snapshotKey(own)
}

// Find returns the eligible artifacts whose logical path matches the glob,
// sorted by artifact id, R nil. A glob without "*" answers from the snapshot's
// logical-path index, built once (4E-R9); the others scan the snapshot.
func (l *lookuper) Find(glob string) []parse.Artifact {
	g, err := parse.CompileGlob(glob)
	if err != nil {
		return nil
	}
	key, literal := literalKey(glob, g)
	if !literal {
		return l.scan(g)
	}
	b := l.b
	var out []parse.Artifact
	for _, c := range b.snap.logicalIndex(b.h)[key] {
		if b.lookupEligible(c.rec) && g.Match(c.logical.Path) {
			out = append(out, l.artifactOf(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// literalKey is the logical path a glob without "*" can match, in the form logical paths have (an
// Android glob allows one optional "/" after the scheme). A relative glob matches nothing here.
func literalKey(glob string, g *parse.Glob) (string, bool) {
	if g.Relative() || strings.Contains(glob, "*") {
		return "", false
	}
	if rest, ok := strings.CutPrefix(glob, "android:"); ok {
		return "android:/" + strings.TrimPrefix(rest, "/"), true
	}
	return glob, true
}

func (l *lookuper) artifactOf(c *candidate) parse.Artifact {
	rec := c.rec
	mb := c.member("", l.b.snap.env)
	return parse.Artifact{
		ID: rec.ID, SHA256: rec.SHA256, Platform: mb.Platform, Logical: mb.Logical,
		Acquisition: acquisitionOf(rec),
		Size:        rec.Size, Incomplete: rec.Incomplete, Source: SourceInfo(rec),
	}
}

// findScan is Find by a linear scan of the snapshot (the reference the index must agree with).
func (l *lookuper) findScan(glob string) []parse.Artifact {
	g, err := parse.CompileGlob(glob)
	if err != nil {
		return nil
	}
	return l.scan(g)
}

func (l *lookuper) scan(g *parse.Glob) []parse.Artifact {
	b := l.b
	var out []parse.Artifact
	for _, rec := range b.snap.records {
		if !b.lookupEligible(rec) {
			continue
		}
		c := b.h.name(rec, b.snap.env)
		if !c.named || !g.Match(c.logical.Path) {
			continue
		}
		out = append(out, l.artifactOf(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Open opens an artifact Find returned: the same eligibility, the same hash
// verification and memory rules as a member, counted against MaxLookupOpens and
// the in-memory total, recorded for the audit, sealed with the bundle. The hash
// runs under the job's context and outside the bundle's lock, so Close and
// Recheck never wait behind it.
func (l *lookuper) Open(a parse.Artifact) (io.ReaderAt, error) {
	b := l.b
	b.mu.Lock()
	if l.sealed() {
		b.mu.Unlock()
		return nil, parse.ErrSealed
	}
	if b.opens >= b.h.limits.MaxLookupOpens {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: the limit is %d", ErrLookupOpens, b.h.limits.MaxLookupOpens)
	}
	rec, ok := b.snap.Record(a.ID)
	if !ok {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, a.ID)
	}
	if !b.lookupEligible(rec) {
		b.mu.Unlock()
		return nil, fmt.Errorf("artparse: artifact %s is not available to a Lookuper", a.ID)
	}
	b.opens++
	s := b.opened[a.ID]
	if s == nil {
		for _, mb := range b.members {
			if mb.src.rec.ID == a.ID {
				s = mb.src // already verified as an input
			}
		}
	}
	var reserved int64 // in-memory budget held for the load below
	if s == nil {
		if rec.Size <= min(b.h.limits.MemInputMax, b.h.limits.MemInputTotal-b.memUsed) {
			reserved = rec.Size
			b.memUsed += reserved
		}
	}
	b.mu.Unlock()

	if s == nil {
		loaded, err := b.h.loadSource(b.ctx, b.snap, a.ID, b.h.limits.MemInputMax, reserved)
		b.mu.Lock()
		defer b.mu.Unlock()
		if err != nil {
			b.memUsed -= reserved
			return nil, err
		}
		switch {
		case b.closed || l.sealed():
			loaded.close()
			b.memUsed -= reserved
			return nil, parse.ErrSealed
		case b.opened[a.ID] != nil: // a concurrent open of the same artifact won
			loaded.close()
			b.memUsed -= reserved
			s = b.opened[a.ID]
		default:
			b.opened[a.ID] = loaded
			s = loaded
		}
		return b.finishOpen(a.ID, s, l.probe, l.own), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finishOpen(a.ID, s, l.probe, l.own), nil
}

// finishOpen records the open and wraps the source. b.mu is held.
func (b *bundle) finishOpen(id string, s *source, probe *parse.ReadBudget, own *sealSet) io.ReaderAt {
	b.openList = append(b.openList, LookupOpen{ArtifactID: id, SHA256: s.rec.SHA256, Streamed: s.f != nil})
	return b.wrapReader(s, probe, own)
}
