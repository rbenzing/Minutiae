package artparse

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// member is one input of a bundle with the facts the parser sees.
type member struct {
	role string
	m    Member
	src  *source
}

// bundle is the verified, sealed inputs of one job. Every artifact was hashed
// against the manifest before any reader existed.
type bundle struct {
	h        *Host
	snap     *Snapshot
	job      Job
	parseID  string
	members  []*member // the primary first, then the other roles by name
	seals    sealSet
	mu       sync.Mutex // guards the lookup state and memUsed
	memUsed  int64
	opened   map[string]*source // artifacts opened through Lookuper, by id
	openList []LookupOpen
	opens    int
	closed   bool
}

// LookupOpen records an artifact a parser opened through its Lookuper, for the
// parse.job.end audit entry.
type LookupOpen struct{ ArtifactID, SHA256 string }

// openBundle verifies and loads every member of job j: the primary first, then
// the other roles by name. On any failure nothing escapes and every handle is
// closed.
func (h *Host) openBundle(ctx context.Context, snap *Snapshot, j Job, parseID string) (*bundle, error) {
	b := &bundle{h: h, snap: snap, job: j, parseID: parseID, opened: map[string]*source{}}
	roles := make([]string, 0, len(j.Others))
	for role := range j.Others {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	b.members = append(b.members, &member{role: j.Parser.meta.Inputs[0].Role, m: j.Primary})
	for _, role := range roles {
		b.members = append(b.members, &member{role: role, m: j.Others[role]})
	}
	for _, mb := range b.members {
		if err := ctx.Err(); err != nil {
			b.close()
			return nil, err
		}
		s, err := h.loadSource(ctx, snap, mb.m.Artifact.ID, h.limits.MemInputMax, h.limits.MemInputTotal-b.memUsed)
		if err != nil {
			b.close()
			return nil, err
		}
		if s.data != nil {
			b.memUsed += s.rec.Size
		}
		mb.src = s
	}
	return b, nil
}

// artifact is the parse.Artifact for a member, R left for the caller.
func (mb *member) artifact() parse.Artifact {
	rec := mb.src.rec
	return parse.Artifact{
		ID: rec.ID, SHA256: rec.SHA256, Platform: mb.m.Platform, Logical: mb.m.Logical,
		Size: rec.Size, Incomplete: rec.Incomplete, Source: SourceInfo(rec),
	}
}

// input returns a fresh Input for one invocation: every value is a copy (it is
// run through Input.Clone), every reader a new sealed wrapper (Probe readers
// carry Limits.ProbeBytes), the Lookuper and the BudgetView new handles. seal
// reaches them all.
func (b *bundle) input(forProbe bool) *parse.Input {
	lim := b.h.limits
	var probe int64
	if forProbe {
		probe = lim.ProbeBytes
	}
	in := &parse.Input{
		Job:       parse.JobInfo{ParseID: b.parseID, Job: b.job.N, Parser: parse.Identity{Name: b.job.Parser.meta.Name, Version: b.job.Parser.meta.Version, Hash: b.job.Parser.hash}},
		Artifacts: map[string]parse.Artifact{},
		Lookup:    &lookuper{b: b, probe: probe},
		Limits:    lim,
		Budget:    parse.NewBudget(lim.MemBudget).View(),
	}
	for i, mb := range b.members {
		a := mb.artifact()
		a.R = b.seals.wrap(mb.src, probe)
		if i == 0 {
			in.Primary = a
		}
		in.Artifacts[mb.role] = a
	}
	return in.Clone()
}

// seal makes every reader of the bundle, the members' and every Lookuper open,
// fail with parse.ErrSealed from now on. Idempotent and atomic.
func (b *bundle) seal() { b.seals.seal() }

// recheck re-hashes every input from disk (the in-memory ones too: the case
// file must still be what the manifest says) and every artifact opened through
// Lookuper. A mismatch is a *HashMismatchError.
func (b *bundle) recheck(ctx context.Context) error {
	for _, mb := range b.members {
		if err := b.h.rehash(ctx, mb.src.rec); err != nil {
			return err
		}
	}
	b.mu.Lock()
	ids := make([]string, 0, len(b.opened))
	for id := range b.opened {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		b.mu.Lock()
		s := b.opened[id]
		b.mu.Unlock()
		if err := b.h.rehash(ctx, s.rec); err != nil {
			return err
		}
	}
	return nil
}

// recheckManifest requires the manifest, read again now, to hold for every
// member and every Lookuper open the record the snapshot holds.
func (b *bundle) recheckManifest(snap *Snapshot) error {
	recs, err := b.h.src.Manifest()
	if err != nil {
		return fmt.Errorf("manifest unreadable: %w", err)
	}
	now := make(map[string][]evidence.ManifestRecord, len(recs))
	for _, r := range recs {
		now[r.ID] = append(now[r.ID], r)
	}
	check := func(id string) error {
		got := now[id]
		if len(got) != 1 || !snap.Matches(got[0]) {
			return integrityf("artifact %s: the manifest changed during the run", id)
		}
		return nil
	}
	for _, mb := range b.members {
		if err := check(mb.src.rec.ID); err != nil {
			return err
		}
	}
	b.mu.Lock()
	var ids []string
	for id := range b.opened {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := check(id); err != nil {
			return err
		}
	}
	return nil
}

// lookups lists what the parser opened through its Lookuper, in open order.
func (b *bundle) lookups() []LookupOpen {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]LookupOpen(nil), b.openList...)
}

// close seals the bundle and releases every handle. Idempotent.
func (b *bundle) close() {
	b.seal()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, mb := range b.members {
		if mb.src != nil {
			mb.src.close()
		}
	}
	for _, s := range b.opened {
		s.close()
	}
}
