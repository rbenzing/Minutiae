package artparse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

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
	probe int64
}

var _ parse.Lookuper = (*lookuper)(nil)

// lookupEligible: live artifacts only (the F6 allowlist without snapshots),
// plus the artifacts of the job's own snapshot when the job is snapshot-derived.
func (b *bundle) lookupEligible(m evidence.ManifestRecord) bool {
	if ok, _ := Eligible(m, false); ok {
		return true
	}
	own := b.job.Primary.Snapshot
	if own == nil {
		return false
	}
	if ok, _ := Eligible(m, true); !ok {
		return false
	}
	d := m.Source.Derived
	return d != nil && d.Snapshot != nil && snapshotKey(&parse.SnapshotInfo{Name: d.Snapshot.Name, Xid: d.Snapshot.Xid}) == snapshotKey(own)
}

// Find returns the eligible artifacts whose logical path matches the glob,
// sorted by artifact id, R nil.
func (l *lookuper) Find(glob string) []parse.Artifact {
	g, err := parse.CompileGlob(glob)
	if err != nil {
		return nil
	}
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
		mb := c.member("", b.snap.env)
		out = append(out, parse.Artifact{
			ID: rec.ID, SHA256: rec.SHA256, Platform: mb.Platform, Logical: mb.Logical,
			Size: rec.Size, Incomplete: rec.Incomplete, Source: SourceInfo(rec),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Open opens an artifact Find returned: the same eligibility, the same hash
// verification and memory rules as a member, counted against MaxLookupOpens and
// recorded for the audit, sealed with the bundle.
func (l *lookuper) Open(a parse.Artifact) (io.ReaderAt, error) {
	b := l.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seals.isSealed() {
		return nil, parse.ErrSealed
	}
	if b.opens >= b.h.limits.MaxLookupOpens {
		return nil, fmt.Errorf("%w: the limit is %d", ErrLookupOpens, b.h.limits.MaxLookupOpens)
	}
	rec, ok := b.snap.Record(a.ID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, a.ID)
	}
	if !b.lookupEligible(rec) {
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
	if s == nil {
		var err error
		s, err = b.h.loadSource(context.Background(), b.snap, a.ID, b.h.limits.MemInputMax, b.h.limits.MemInputTotal-b.memUsed)
		if err != nil {
			return nil, err
		}
		if s.data != nil {
			b.memUsed += s.rec.Size
		}
		b.opened[a.ID] = s
	}
	b.openList = append(b.openList, LookupOpen{ArtifactID: a.ID, SHA256: s.rec.SHA256})
	return b.seals.wrap(s, l.probe), nil
}
