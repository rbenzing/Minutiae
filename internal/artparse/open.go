package artparse

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	probed   sealSet         // the readers and Lookupers made for Probe inputs: sealed when Probe returns, without sealing the bundle
	ctx      context.Context // the job context: every hash of a lookup open runs under it
	mu       sync.Mutex      // guards the lookup state and memUsed (never held while hashing)
	memUsed  int64
	opened   map[string]*source // artifacts opened through Lookuper, by id
	openList []LookupOpen
	opens    int
	closed   bool
}

// LookupOpen records an artifact a parser opened through its Lookuper, for the
// parse.job.end audit entry.
type LookupOpen struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
	Streamed   bool   `json:"streamed"` // served from the file (pre- and post-hashed) rather than from memory
}

// openBundle verifies and loads every member of job j: the primary first, then
// the other roles by name. On any failure nothing escapes and every handle is
// closed.
func (h *Host) openBundle(ctx context.Context, snap *Snapshot, j Job, parseID string) (*bundle, error) {
	b := &bundle{h: h, ctx: ctx, snap: snap, job: j, parseID: parseID, opened: map[string]*source{}}
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
	var probe *parse.ReadBudget // one budget per Probe call, shared by every reader of this Input
	var own *sealSet            // sealed on its own when Probe returns
	if forProbe {
		probe = parse.NewReadBudget(lim.ProbeBytes)
		own = &b.probed
	}
	in := &parse.Input{
		Job:       parse.JobInfo{ParseID: b.parseID, Job: b.job.N, Parser: parse.Identity{Name: b.job.Parser.meta.Name, Version: b.job.Parser.meta.Version, Hash: b.job.Parser.hash}},
		Artifacts: map[string]parse.Artifact{},
		Lookup:    &lookuper{b: b, probe: probe, own: own},
		Limits:    lim,
		Budget:    parse.NewBudget(lim.MemBudget).View(),
	}
	for i, mb := range b.members {
		a := mb.artifact()
		a.R = b.wrapReader(mb.src, probe, own)
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

// sealProbe makes every reader and Lookuper made for a Probe input fail with parse.ErrSealed from now
// on, and leaves the bundle open for the inputs of the Parse that follows.
func (b *bundle) sealProbe() { b.probed.seal() }

// wrapReader hands out a sealed reader over r that the bundle seal reaches, and the set own (when not
// nil) too.
func (b *bundle) wrapReader(r io.ReaderAt, budget *parse.ReadBudget, own *sealSet) *parse.SealedReaderAt {
	w := b.seals.wrap(r, budget)
	if own != nil {
		own.add(w)
	}
	return w
}

// recheck re-hashes every input from disk (the in-memory ones too: the case
// file must still be what the manifest says) and every artifact opened through
// Lookuper. A mismatch is a *HashMismatchError.
func (b *bundle) recheck(ctx context.Context) error {
	recs := make([]evidence.ManifestRecord, 0, len(b.members))
	for _, mb := range b.members {
		recs = append(recs, mb.src.rec)
	}
	b.mu.Lock()
	opened := make([]evidence.ManifestRecord, 0, len(b.opened))
	for _, s := range b.opened {
		opened = append(opened, s.rec)
	}
	b.mu.Unlock()
	sort.Slice(opened, func(i, j int) bool { return opened[i].ID < opened[j].ID })
	recs = append(recs, opened...)
	want := make([]parse.Artifact, len(recs))
	for i, rec := range recs {
		want[i] = parse.Artifact{ID: rec.ID, SHA256: rec.SHA256}
	}
	paths := map[string]string{} // the case path each hash was read from, for the error
	err := parse.CheckInputsUnchanged(want, func(a parse.Artifact) (string, error) {
		var rec evidence.ManifestRecord
		for _, r := range recs {
			if r.ID == a.ID {
				rec = r
			}
		}
		hash, path, err := b.h.hashNow(ctx, rec)
		paths[a.ID] = path
		return hash, err
	})
	var changed *parse.InputChangedError
	if errors.As(err, &changed) {
		return &HashMismatchError{ArtifactID: changed.ArtifactID, Path: paths[changed.ArtifactID], Want: changed.Want, Got: changed.Got}
	}
	return err
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

// streamed lists the ids of the members served from the file rather than from memory: for these
// the post-job re-hash is the only defence against a change that is undone before it (job.end
// records them).
func (b *bundle) streamed() []string {
	var ids []string
	for _, mb := range b.members {
		if mb.src.f != nil {
			ids = append(ids, mb.src.rec.ID)
		}
	}
	return ids
}
