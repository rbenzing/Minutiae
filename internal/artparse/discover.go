package artparse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/all" // registers the payload validators Check relies on
)

// Options configures a Host. The unexported fields are test seams, set only from
// export_test.go.
type Options struct {
	Limits parse.Limits
	Namers []Namer // nil = DefaultNamers()

	skipLimitMinimums bool          // accept Timeout, GracePeriod and ProbeTimeout below the production minimums
	source            caseSource    // replaces the case as the reader of the manifest and audit log
	newWriter         writerFactory // replaces records.NewWriter
	onOpen            func(artifactID string)
	afterStart        func()
}

// Host holds the case, the registry and the options.
type Host struct {
	c         *evidence.Case
	src       caseSource
	registry  []Registered
	namers    []Namer
	limits    parse.Limits
	newWriter writerFactory
	onOpen    func(artifactID string)
	afterSt   func()
}

// Production minimums that skipLimitMinimums lowers (the same numbers
// parse.Limits.Validate enforces).
const (
	minTimeout      = time.Second
	minGracePeriod  = 10 * time.Millisecond
	minProbeTimeout = 100 * time.Millisecond
)

// New makes a host over a case and a registry. Names must be unique, the limits
// must pass parse.Limits.Validate (the production minimums) and every parser must
// pass Check.
func New(c *evidence.Case, ps []Registered, opt Options) (*Host, error) {
	if c == nil {
		return nil, errors.New("artparse: no case")
	}
	lim := opt.Limits
	if opt.skipLimitMinimums {
		lim.Timeout = max(lim.Timeout, minTimeout)
		lim.GracePeriod = max(lim.GracePeriod, minGracePeriod)
		lim.ProbeTimeout = max(lim.ProbeTimeout, minProbeTimeout)
	}
	if err := lim.Validate(); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, r := range ps {
		if r.p == nil {
			return nil, errors.New("artparse: a parser was not made by Register")
		}
		if names[r.meta.Name] {
			return nil, fmt.Errorf("artparse: two parsers are named %q", r.meta.Name)
		}
		names[r.meta.Name] = true
		if err := r.Check(); err != nil {
			return nil, err
		}
	}
	h := &Host{
		c: c, registry: slices.Clone(ps), namers: opt.Namers, limits: opt.Limits,
		src: opt.source, newWriter: opt.newWriter, onOpen: opt.onOpen, afterSt: opt.afterStart,
	}
	if h.src == nil {
		h.src = c
	}
	if h.namers == nil {
		h.namers = DefaultNamers()
	}
	if h.newWriter == nil {
		h.newWriter = newRecordsWriter
	}
	return h, nil
}

// ParserRef selects a parser by name, optionally pinned to a version.
type ParserRef struct{ Name, Version string }

// Selection narrows discovery.
type Selection struct {
	Parsers          []ParserRef // empty = every parser
	Artifacts        []string    // empty = every eligible artifact
	Reparse          bool
	IncludeSnapshots bool
}

// ErrSelection is an unknown parser or a NAME@VERSION this build does not hold.
var ErrSelection = errors.New("invalid selection")

// Member is one artifact of a bundle.
type Member struct {
	Role                     string
	Artifact                 evidence.ManifestRecord
	Logical, Platform, Namer string
	Snapshot                 *parse.SnapshotInfo
}

// Job statuses Discover produces.
const (
	StatusNew           = "new"
	StatusAlreadyParsed = "already-parsed"
	StatusStaleBundle   = "stale-bundle"
	StatusUnparsed      = "unparsed"
)

// Job is one parser over one bundle of artifacts of one acquisition.
type Job struct {
	N        int
	Parser   Registered
	Primary  Member
	Others   map[string]Member
	Explicit bool   // the primary was named with --artifact (no glob match, Probe decides)
	Status   string // new | already-parsed | stale-bundle | unparsed | skipped | refused
	Reason   string
}

// Counts tell what discovery saw.
type Counts struct {
	Manifest, NotParserInput, SnapshotExcluded int
	ByKind                                     map[string]int // every manifest record, by source kind
}

// candidate is a manifest record with the logical path a namer gave it.
type candidate struct {
	rec      evidence.ManifestRecord
	logical  Logical
	named    bool
	snapshot *parse.SnapshotInfo
	group    string // acquisition and snapshot: a bundle never crosses it
}

func (c *candidate) member(role string, env Env) Member {
	m := Member{Role: role, Artifact: c.rec, Logical: c.logical.Path, Platform: c.logical.Platform, Namer: c.logical.Namer, Snapshot: c.snapshot}
	if !c.named {
		switch c.rec.Source.Kind {
		case "backup":
			m.Platform = parse.PlatformIOS
		case "file":
			m.Platform = env.DevicePlatform[c.rec.Source.DeviceID]
		}
	}
	return m
}

// acquisitionKey is the first three components of the manifest path
// (artifacts/<device>/<acquisition>).
func acquisitionKey(p string) string {
	parts := strings.Split(p, "/")
	n := min(3, len(parts)-1)
	return strings.Join(parts[:max(n, 0)], "/")
}

func snapshotKey(s *parse.SnapshotInfo) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%d", s.Name, s.Xid)
}

// Discover builds the jobs of a selection from the snapshot alone: it never
// opens an artifact and never re-reads the manifest.
func (h *Host) Discover(ctx context.Context, snap *Snapshot, sel Selection) ([]Job, Counts, error) {
	counts := Counts{Manifest: len(snap.records), ByKind: map[string]int{}}
	var cands []*candidate
	for _, m := range snap.records {
		counts.ByKind[m.Source.Kind]++
		ok, _, snapExcluded := classify(m, sel.IncludeSnapshots)
		switch {
		case snapExcluded:
			counts.SnapshotExcluded++
		case !ok:
			counts.NotParserInput++
		default:
			cands = append(cands, h.name(m, snap.env))
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].rec.ID < cands[j].rec.ID })
	byID := make(map[string]*candidate, len(cands))
	groups := map[string][]*candidate{}
	for _, c := range cands {
		byID[c.rec.ID] = c
		groups[c.group] = append(groups[c.group], c)
	}

	parsers, err := h.selectParsers(sel.Parsers)
	if err != nil {
		return nil, counts, err
	}
	var jobs []Job
	if len(sel.Artifacts) > 0 {
		explicit, err := h.explicitCandidates(snap, byID, sel)
		if err != nil {
			return nil, counts, err
		}
		for _, r := range parsers {
			for _, c := range explicit {
				if job, ok := h.bundle(r, c, groups, snap.env, true); ok {
					jobs = append(jobs, job)
				}
			}
		}
	} else {
		for _, r := range parsers {
			globs, err := compilePrimary(r)
			if err != nil {
				return nil, counts, err
			}
			for _, c := range cands {
				if c.named && matchesAny(globs, c.logical.Path) {
					if job, ok := h.bundle(r, c, groups, snap.env, false); ok {
						jobs = append(jobs, job)
					}
				}
			}
		}
	}
	for i := range jobs {
		jobs[i].N = i + 1
		if jobs[i].Status == "" {
			if err := h.statusFromRuns(ctx, &jobs[i]); err != nil {
				return nil, counts, err
			}
		}
	}
	return jobs, counts, nil
}

// name gives a record its logical path: the first namer that answers.
func (h *Host) name(m evidence.ManifestRecord, env Env) *candidate {
	c := &candidate{rec: m}
	if d := m.Source.Derived; d != nil && d.Snapshot != nil {
		c.snapshot = &parse.SnapshotInfo{Name: d.Snapshot.Name, Xid: d.Snapshot.Xid}
	}
	c.group = acquisitionKey(m.Path) + "\x01" + snapshotKey(c.snapshot)
	for _, n := range h.namers {
		if l, ok := n.Logical(m, env); ok {
			c.logical, c.named = l, true
			break
		}
	}
	return c
}

func (h *Host) selectParsers(refs []ParserRef) ([]Registered, error) {
	if len(refs) == 0 {
		return h.registry, nil
	}
	want := map[string]bool{}
	for _, ref := range refs {
		var found bool
		for _, r := range h.registry {
			if r.meta.Name == ref.Name && (ref.Version == "" || r.meta.Version == ref.Version) {
				found = true
				want[r.meta.Name] = true
			}
		}
		if !found {
			if ref.Version != "" {
				return nil, fmt.Errorf("%w: this build holds no parser %s@%s", ErrSelection, ref.Name, ref.Version)
			}
			return nil, fmt.Errorf("%w: unknown parser %q", ErrSelection, ref.Name)
		}
	}
	var out []Registered
	for _, r := range h.registry {
		if want[r.meta.Name] {
			out = append(out, r)
		}
	}
	return out, nil
}

// explicitCandidates resolves --artifact ids: each must be in the manifest and
// eligible. Duplicates collapse; the result is in artifact id order.
func (h *Host) explicitCandidates(snap *Snapshot, byID map[string]*candidate, sel Selection) ([]*candidate, error) {
	seen := map[string]bool{}
	var out []*candidate
	for _, id := range sel.Artifacts {
		m, ok := snap.Record(id)
		if !ok {
			return nil, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, id)
		}
		if ok, why := Eligible(m, sel.IncludeSnapshots); !ok {
			return nil, fmt.Errorf("%w: %s: %s", ErrNotParserInput, id, why)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, byID[id])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rec.ID < out[j].rec.ID })
	return out, nil
}

func compilePrimary(r Registered) ([]*parse.Glob, error) {
	var out []*parse.Glob
	for _, pat := range r.meta.Inputs[0].Globs {
		g, err := parse.CompileGlob(pat)
		if err != nil {
			return nil, fmt.Errorf("parser %s: %w", r.meta.Name, err)
		}
		out = append(out, g)
	}
	return out, nil
}

func matchesAny(gs []*parse.Glob, logical string) bool {
	for _, g := range gs {
		if g.Match(logical) {
			return true
		}
	}
	return false
}

// bundle builds the job of parser r over primary p: for every other role the
// artifacts of the same acquisition and the same snapshot (or none) that match
// its relative globs or companion suffixes. ok is false when a required role has
// no match (and the job is not explicit).
func (h *Host) bundle(r Registered, p *candidate, groups map[string][]*candidate, env Env, explicit bool) (Job, bool) {
	job := Job{Parser: r, Primary: p.member(r.meta.Inputs[0].Role, env), Others: map[string]Member{}, Explicit: explicit}
	var missing []string
	for _, in := range r.meta.Inputs[1:] {
		var matches []*candidate
		if p.named {
			matches = roleMatches(in, p, groups[p.group])
		}
		switch {
		case len(matches) == 0 && in.Required:
			missing = append(missing, in.Role)
		case len(matches) == 1:
			job.Others[in.Role] = matches[0].member(in.Role, env)
		case len(matches) > 1:
			ids := make([]string, len(matches))
			for i, m := range matches {
				ids[i] = m.rec.ID
			}
			job.Status = StatusUnparsed
			job.Reason = appendReason(job.Reason, fmt.Sprintf("ambiguous companion for role %s: %s", in.Role, strings.Join(ids, ", ")))
		}
	}
	if len(missing) > 0 {
		if !explicit {
			return Job{}, false
		}
		job.Status = StatusUnparsed
		job.Reason = appendReason(job.Reason, "required role not found: "+strings.Join(missing, ", "))
	}
	return job, true
}

func appendReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// roleMatches returns the candidates of one group that fill role in for primary
// p, in artifact id order (the group is sorted by id).
func roleMatches(in parse.InputSpec, p *candidate, group []*candidate) []*candidate {
	var globs []*parse.Glob
	for _, pat := range in.Globs {
		if g, err := parse.CompileGlob(pat); err == nil {
			globs = append(globs, g)
		}
	}
	var out []*candidate
	for _, c := range group {
		if c == p || !c.named {
			continue
		}
		hit := false
		for _, g := range globs {
			if g.MatchRelative(p.logical.Path, c.logical.Path) {
				hit = true
			}
		}
		for _, suffix := range in.Companions {
			if c.logical.Path == p.logical.Path+suffix {
				hit = true
			}
		}
		if hit {
			out = append(out, c)
		}
	}
	return out
}

// statusFromRuns sets new, already-parsed or stale-bundle from the runs the
// records database holds over the bundle's artifacts.
func (h *Host) statusFromRuns(ctx context.Context, j *Job) error {
	members := []Member{j.Primary}
	roles := make([]string, 0, len(j.Others))
	for role := range j.Others {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		members = append(members, j.Others[role])
	}
	var uncovered []string
	for _, m := range members {
		done, err := records.AlreadyIngested(ctx, h.c, m.Artifact.ID, j.Parser.Identity())
		if err != nil {
			return err
		}
		if !done {
			uncovered = append(uncovered, m.Artifact.ID)
		}
	}
	switch {
	case len(uncovered) == len(members):
		j.Status = StatusNew
	case len(uncovered) == 0:
		j.Status = StatusAlreadyParsed
	default:
		j.Status = StatusStaleBundle
		j.Reason = "not covered by an earlier run: " + strings.Join(uncovered, ", ")
	}
	return nil
}
