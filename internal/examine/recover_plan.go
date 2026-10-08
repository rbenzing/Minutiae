package examine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// The recovery planner (spec 6.2): it enumerates the deleted entries, asks the filesystem's Recoverer for
// maps, validates every map against the free space and the image, selects, caps and admits candidates
// to the output budget. It is pure arithmetic over reads: PlanRecovery writes nothing and audits
// nothing; Recover audits the plan and writes it.

// Limits of one recovery planning pass.
const (
	// MaxRecoverEntries caps the deleted entries one run considers; the rest are counted as limit.
	MaxRecoverEntries = 1_000_000
	// maxPlanRuns caps the runs the plan holds over all candidates; the rest are counted as limit.
	maxPlanRuns = 1 << 22
	// maxExcludedListed is how many excluded runs Recovery.Excluded lists (the counters are exact).
	maxExcludedListed = 256
)

// Seams for the cap boundary tests (the real values are far too large to build in a test).
var (
	recoverEntryCap = MaxRecoverEntries
	planRunCap      = maxPlanRuns
)

// RecoverOptions select what Recover (and PlanRecovery) considers.
type RecoverOptions struct {
	Partition     int      // -1 = auto
	Refs          []string // "id:<fs id>" of deleted entries, or live directory paths (their whole subtree); empty with All = "/"
	All           bool
	MinConfidence int // 0..100; candidates below are skipped (below-min-confidence), never written
	AllCandidates bool
	KeepUniform   bool
	MaxFiles      int   // 0 = DefaultMaxFiles
	MaxBytes      int64 // 0 = DefaultMaxBytes
	Progress      func(done, total int64)
}

// PlannedCandidate is one recovery map after validation against the free space.
type PlannedCandidate struct {
	Ordinal            int // 0 until chosen for writing
	Method             string
	Confidence         int
	Band               string
	DeclaredSize, Size int64 // Size = captured bytes
	Runs, Excluded     []evidence.Run
	Alloc              evidence.AllocSummary
	Basis, Assumptions []string
	Content            string // "ok" | "uniform"
	Encrypted          bool
	Mode               uint32
	Times              filesys.Times
	Skip               string // "" = will be written; else a reason token

	skipDetail string
	cut        bool   // some runs were not free
	cutState   string // state of the first non-free byte
	cutRuns    int    // runs not (fully) captured
	mapRuns    int    // runs of the map
	errText    string // Error of the artifact when it is partial
}

// PlanSkip is one entry or candidate that will not be written, and why.
type PlanSkip struct{ Path, ID, Reason, Detail string }

// RecoverItem is one deleted entry with its candidates.
type RecoverItem struct {
	Path, ID, Name string
	Type           filesys.EntryType
	Candidates     []PlannedCandidate // best first
	Skips          []PlanSkip

	notes []PlanSkip // warnings that are not skips (a cut candidate list)
	enc   bool       // the listing flagged the entry encrypted
}

// RecoverPlan is what a recovery run would do.
type RecoverPlan struct {
	Items        []RecoverItem
	Considered   int
	SkippedBy    map[string]int
	LimitReached string // "" | "max-files" | "max-bytes" | "max-entries" | "max-plan-runs": the first limit met
	NotProcessed int
	FSWarnings   []string // new filesystem warnings the planning raised (the unallocated note included)

	candidates  int
	uniform     int
	overlap     int
	unallocNote string
	limits      []PlanSkip // one per limit met
	part        volume.Partition
	fsType      string
	admitted    int64 // bytes of the candidates to write
}

// RecoverSummary is the outcome of a recovery run.
type RecoverSummary struct {
	Summary                // AnalysisID, Files (complete artifacts), Bytes, Skipped, Warnings, Artifacts (sidecars included), FSWarnings
	Considered, Candidates int
	Recovered, Partial     int // artifacts written (sidecars excluded); Partial = flagged incomplete
	Uniform, Overlap       int
	SkippedBy              map[string]int
	LimitReached           string // see RecoverPlan.LimitReached
	NotProcessed           int
}

// recovererOf returns the Recoverer of fsys, or filesys.ErrNoRecovery. Recover calls it before any audit
// entry is written.
func (s *Session) recovererOf(fsys filesys.FileSystem) (filesys.Recoverer, error) {
	if !filesys.SupportsRecovery(fsys) {
		return nil, filesys.ErrNoRecovery
	}
	if r, ok := fsys.(filesys.Recoverer); ok {
		return r, nil
	}
	if r, ok := filesys.As[filesys.Recoverer](fsys); ok {
		return r, nil
	}
	return nil, filesys.ErrNoRecovery
}

func (o RecoverOptions) validate() error {
	if o.MinConfidence < 0 || o.MinConfidence > 100 {
		return fmt.Errorf("min confidence %d is outside 0..100", o.MinConfidence)
	}
	if !o.All && len(o.Refs) == 0 {
		return errors.New("nothing to recover: give entries or directories, or ask for all")
	}
	return nil
}

// PlanRecovery is read-only: it appends nothing to the audit log and writes nothing.
func (s *Session) PlanRecovery(ctx context.Context, o RecoverOptions) (*RecoverPlan, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	b, err := newBudget(o.MaxFiles, o.MaxBytes)
	if err != nil {
		return nil, err
	}
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return nil, err
	}
	rec, err := s.recovererOf(fsys)
	if err != nil {
		return nil, err
	}
	before := fsys.Info().Warnings
	plan, err := s.buildPlan(ctx, o, fsys, rec, part, b)
	if err != nil {
		return nil, err
	}
	for _, w := range fsys.Info().Warnings {
		if !slices.Contains(before, w) {
			plan.FSWarnings = append(plan.FSWarnings, w)
		}
	}
	if plan.unallocNote != "" {
		plan.FSWarnings = append(plan.FSWarnings, plan.unallocNote)
	}
	return plan, nil
}

// target is one deleted entry to consider.
type target struct {
	path string
	e    filesys.Entry
}

type planner struct {
	s      *Session
	ctx    context.Context
	o      RecoverOptions
	fsys   filesys.FileSystem
	rec    filesys.Recoverer
	part   volume.Partition
	free   *freeMap
	plan   *RecoverPlan
	budget *budget
}

func (p *planner) skipped(reason string) { p.plan.SkippedBy[reason]++ }

// buildPlan is the whole planning pass shared by PlanRecovery and Recover.
func (s *Session) buildPlan(ctx context.Context, o RecoverOptions, fsys filesys.FileSystem, rec filesys.Recoverer, part volume.Partition, b *budget) (*RecoverPlan, error) {
	runs, note, err := s.unallocatedRunsNote(fsys, part, false)
	if err != nil {
		return nil, err
	}
	// The allocation state of a byte that is not free is "unknown" when the filesystem has raised any
	// warning (decided by presence, so a list and a run agree whatever ran before).
	unknown := len(fsys.Info().Warnings) > 0
	p := &planner{
		s: s, ctx: ctx, o: o, fsys: fsys, rec: rec, part: part, free: newFreeMap(runs, unknown), budget: b,
		plan: &RecoverPlan{SkippedBy: map[string]int{}, unallocNote: note, part: part, fsType: fsys.Info().Type},
	}
	targets, over, err := p.enumerate()
	if err != nil {
		return nil, err
	}
	p.plan.Considered = len(targets) + over
	if over > 0 {
		p.limit("max-entries", over, fmt.Sprintf("more than %d deleted entries; %d were not processed", recoverEntryCap, over))
	}
	if err := p.items(targets); err != nil {
		return nil, err
	}
	p.selectAndFilter()
	if err := p.admit(); err != nil {
		return nil, err
	}
	return p.plan, nil
}

// limit records that a cap cut n candidates or entries off. The first limit met is LimitReached.
func (p *planner) limit(which string, n int, detail string) {
	if p.plan.LimitReached == "" {
		p.plan.LimitReached = which
	}
	p.plan.NotProcessed += n
	p.plan.SkippedBy["limit"] += n
	p.plan.limits = append(p.plan.limits, PlanSkip{Reason: "limit", Detail: which + ": " + detail})
}

// enumerate returns the deleted entries to consider, deduplicated by (path, id), at most
// recoverEntryCap of them, and how many more were met.
func (p *planner) enumerate() (targets []target, over int, err error) {
	refs := slices.Clone(p.o.Refs)
	if p.o.All {
		refs = append([]string{"/"}, refs...)
	}
	seen := map[[2]string]bool{}
	add := func(t target) {
		k := [2]string{t.path, t.e.ID}
		if seen[k] {
			return
		}
		seen[k] = true
		if len(targets) >= recoverEntryCap {
			over++
			return
		}
		targets = append(targets, t)
	}
	var skips []PlanSkip // walk errors: entries that could not be listed
	collect := func(root filesys.Entry, rootPath string) error {
		return filesys.Walk(p.fsys, root, rootPath, func(wp string, we filesys.Entry, werr error) error {
			if err := p.ctx.Err(); err != nil {
				return err
			}
			if werr != nil {
				skips = append(skips, PlanSkip{Path: wp, ID: we.ID, Reason: "corrupt", Detail: werr.Error()})
				return nil
			}
			if filesys.IsSnapshotsDir(we) {
				return filesys.SkipDir
			}
			if we.Deleted {
				add(target{wp, we})
			}
			return nil
		})
	}
	// One root walk collects every wanted id.
	wanted := map[string]bool{}
	for _, r := range refs {
		if id, ok := strings.CutPrefix(r, "id:"); ok {
			if id == "" {
				return nil, 0, errors.New("empty entry id after \"id:\"")
			}
			wanted[id] = true
		}
	}
	deletedByID := map[string][]target{}
	liveID := map[string]bool{}
	if len(wanted) > 0 {
		err := filesys.Walk(p.fsys, p.fsys.Root(), "/", func(wp string, we filesys.Entry, werr error) error {
			if err := p.ctx.Err(); err != nil {
				return err
			}
			if werr != nil || !wanted[we.ID] {
				return nil
			}
			if we.Deleted {
				deletedByID[we.ID] = append(deletedByID[we.ID], target{wp, we})
			} else {
				liveID[we.ID] = true
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		if wanted[p.fsys.Root().ID] {
			liveID[p.fsys.Root().ID] = true
		}
	}
	for _, r := range refs {
		if id, ok := strings.CutPrefix(r, "id:"); ok {
			switch ts := deletedByID[id]; {
			case len(ts) > 0:
				for _, t := range ts {
					add(t)
				}
			case liveID[id]:
				return nil, 0, fmt.Errorf("id:%s: %w", id, filesys.ErrNotDeleted)
			default:
				return nil, 0, fmt.Errorf("id:%s: %w", id, filesys.ErrNotFound)
			}
			continue
		}
		var (
			e  filesys.Entry
			ep string
		)
		if cp := path.Clean("/" + r); cp == "/" {
			e, ep = p.fsys.Root(), "/"
		} else {
			var lerr error
			if e, lerr = p.fsys.Lookup(cp); lerr != nil {
				return nil, 0, fmt.Errorf("%s: %w", cp, lerr)
			}
			ep = cp
		}
		switch {
		case e.Deleted:
			add(target{ep, e})
		case e.Type == filesys.TypeDir:
			if err := collect(e, ep); err != nil {
				return nil, 0, err
			}
		default:
			return nil, 0, fmt.Errorf("%s: %w", ep, filesys.ErrNotDeleted)
		}
	}
	for _, sk := range skips {
		p.plan.Items = append(p.plan.Items, RecoverItem{Path: sk.Path, ID: sk.ID, Skips: []PlanSkip{sk}})
		p.skipped(sk.Reason)
	}
	return targets, over, nil
}

// entrySkip records that a whole entry is not recovered.
func (p *planner) entrySkip(it *RecoverItem, reason, detail string) {
	it.Skips = append(it.Skips, PlanSkip{Path: it.Path, ID: it.ID, Reason: reason, Detail: detail})
	p.skipped(reason)
}

// items asks the Recoverer about every target and validates its maps.
func (p *planner) items(targets []target) error {
	// An id held by more than one deleted entry is ambiguous: none of them is recovered.
	byID := map[string][]string{}
	for _, t := range targets {
		byID[t.e.ID] = append(byID[t.e.ID], t.path)
	}
	runsHeld := 0
	for i, t := range targets {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		it := RecoverItem{Path: t.path, ID: t.e.ID, Name: t.e.Name, Type: t.e.Type, enc: t.e.Encrypted}
		switch {
		case len(byID[t.e.ID]) > 1:
			p.entrySkip(&it, "duplicate-id", fmt.Sprintf("the id %q is held by %d deleted entries: %s", t.e.ID, len(byID[t.e.ID]), strings.Join(byID[t.e.ID], ", ")))
		case t.e.Type != filesys.TypeFile && t.e.Type != filesys.TypeSymlink:
			p.entrySkip(&it, "not-a-file", "a deleted "+t.e.Type.String()+" is not recovered")
		default:
			cs, err := p.rec.Recoverable(t.e)
			switch {
			case err == nil:
			case errors.Is(err, filesys.ErrNotFound):
				p.entrySkip(&it, "not-found", err.Error())
			case errors.Is(err, filesys.ErrNotDeleted):
				p.entrySkip(&it, "not-deleted", err.Error())
			case errors.Is(err, filesys.ErrCorrupt):
				p.entrySkip(&it, "corrupt", err.Error())
			case errors.Is(err, filesys.ErrUnsupported), errors.Is(err, filesys.ErrEncrypted):
				p.entrySkip(&it, "unsupported", err.Error())
			default:
				return err // an I/O error is not corruption: the run ends
			}
			if err == nil && len(cs) == 0 {
				p.entrySkip(&it, "no-map", "the filesystem offers no recovery map")
			} else if err == nil {
				if len(cs) > filesys.MaxCandidatesPerEntry {
					it.notes = append(it.notes, PlanSkip{
						Path: t.path, ID: t.e.ID, Reason: "candidate-cap",
						Detail: fmt.Sprintf("%d candidates offered; only the first %d are considered", len(cs), filesys.MaxCandidatesPerEntry),
					})
					cs = cs[:filesys.MaxCandidatesPerEntry]
				}
				for _, c := range cs {
					runsHeld += len(c.Runs)
				}
				if runsHeld > planRunCap {
					rest := len(targets) - i
					p.limit("max-plan-runs", rest, fmt.Sprintf("the plan would hold more than %d runs; %d entries were not processed", planRunCap, rest))
					return nil
				}
				p.evaluate(&it, t, cs)
			}
		}
		p.plan.Items = append(p.plan.Items, it)
	}
	return nil
}

// evaluate validates the candidates of one entry (spec: A11) and orders them best first.
func (p *planner) evaluate(it *RecoverItem, t target, cs []filesys.Candidate) {
	var valid, invalid []PlannedCandidate
	imageSize := p.s.Image.Size()
	for _, c := range cs {
		p.plan.candidates++
		pc := PlannedCandidate{Method: c.Method, DeclaredSize: c.Size, Encrypted: c.Encrypted || t.e.Encrypted, Mode: c.Mode, Times: c.Times, mapRuns: len(c.Runs)}
		reject := func(reason, detail string) {
			pc.Skip, pc.skipDetail = reason, detail
			invalid = append(invalid, pc)
			p.skipped(reason)
		}
		if _, err := filesys.CheckCandidate(c, p.part.Length); err != nil {
			reject("invalid-map", err.Error())
			continue
		}
		if _, ok := confidenceFor(c.Method, false, false); !ok {
			reject("unknown-method", fmt.Sprintf("method %q has no confidence rule in this build", c.Method))
			continue
		}
		if c.Size > 0 && len(c.Runs) == 0 {
			reject("no-runs", fmt.Sprintf("the map declares %d bytes and has no runs", c.Size))
			continue
		}
		imgRuns := make([]evidence.Run, 0, len(c.Runs))
		bad := ""
		for i, r := range c.Runs {
			off, ok := filesys.AddOK(r.Offset, p.part.Start)
			end, ok2 := filesys.AddOK(off, r.Length)
			if !ok || !ok2 || end > imageSize {
				bad = fmt.Sprintf("run %d (%d+%d) lies beyond the %d-byte image", i, r.Offset, r.Length, imageSize)
				break
			}
			imgRuns = append(imgRuns, evidence.Run{Offset: off, Length: r.Length})
		}
		if bad != "" {
			reject("invalid-map", bad)
			continue
		}
		cut := cutAtFirstNonFree(p.free, imgRuns)
		if cut.CapturedBytes == 0 {
			state := cut.State
			if state == "" {
				state = "empty map"
			}
			reject("no-free-bytes", "no byte of the map is free ("+state+")")
			continue
		}
		pc.Size, pc.Runs = cut.CapturedBytes, cut.Captured
		pc.Excluded = slices.Clone(cut.Excluded[:min(len(cut.Excluded), maxExcludedListed)])
		pc.Alloc = evidence.AllocSummary{Free: cut.CapturedBytes, ExcludedRuns: len(cut.Excluded), ExcludedBytes: cut.ExcludedBytes}
		pc.cut, pc.cutState, pc.cutRuns = cut.State != "", cut.State, len(cut.Excluded)
		pc.Basis = cleanList(c.Basis)
		pc.Assumptions = cleanList(c.Assumptions)
		if pc.Size < c.Size {
			pc.Assumptions = append(pc.Assumptions, fmt.Sprintf("size-declared=%d", c.Size))
		}
		if pc.cut {
			pc.Assumptions = append(pc.Assumptions, "prefix-only", "cut-state="+pc.cutState)
			why := "allocated since deletion"
			if pc.cutState == "unknown" {
				why = "allocation state unknown"
			}
			pc.errText = fmt.Sprintf("%d of %d runs not captured: %s", pc.cutRuns, pc.mapRuns, why)
		} else if pc.Size < c.Size {
			pc.errText = fmt.Sprintf("the map covers %d of %d declared bytes", pc.Size, c.Size)
		}
		pc.Content = "ok"
		pc.Confidence, _ = confidenceFor(c.Method, pc.cut, false)
		pc.Band = evidence.ConfidenceBand(pc.Confidence)
		valid = append(valid, pc)
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Confidence > valid[j].Confidence })
	it.Candidates = append(valid, invalid...)
}

// selectAndFilter applies the selection rules in the order of spec A9: the best candidate of an entry
// (or all of them), the overlap between the selected candidates of different entries, then the minimum
// confidence.
func (p *planner) selectAndFilter() {
	var owned []ownedRuns
	for i := range p.plan.Items {
		it := &p.plan.Items[i]
		first := true
		for j := range it.Candidates {
			c := &it.Candidates[j]
			if c.Skip != "" {
				continue
			}
			if !first && !p.o.AllCandidates {
				c.Skip, c.skipDetail = "not-selected", "a better candidate of this entry was selected"
				p.skipped("not-selected")
				continue
			}
			first = false
			owned = append(owned, ownedRuns{Owner: it.ID, Runs: c.Runs})
		}
	}
	overlaps := overlapsOf(owned)
	for i := range p.plan.Items {
		it := &p.plan.Items[i]
		partners := overlaps[it.ID]
		for j := range it.Candidates {
			c := &it.Candidates[j]
			if c.Skip != "" {
				continue
			}
			if len(partners) > 0 {
				p.plan.overlap++
				for _, o := range partners {
					c.Assumptions = append(c.Assumptions, "overlap="+cleanText(o, 200))
				}
				if len(partners) >= maxOverlapListed {
					// overlapsOf lists the smallest 16 only; whether there are more is not known.
					c.Assumptions = append(c.Assumptions, fmt.Sprintf("overlap-listed=%d", maxOverlapListed))
				}
			}
			c.Confidence, _ = confidenceFor(c.Method, c.cut, len(partners) > 0)
			c.Band = evidence.ConfidenceBand(c.Confidence)
			c.Assumptions = fitAssumptions(c.Assumptions)
			if c.Confidence < p.o.MinConfidence {
				c.Skip, c.skipDetail = "below-min-confidence", fmt.Sprintf("confidence %d is below the minimum %d", c.Confidence, p.o.MinConfidence)
				p.skipped("below-min-confidence")
			}
		}
	}
}

// fitAssumptions keeps the list inside the record limit (64 entries), dropping the reader's own entries
// (the first ones) before the ones the planner added last.
func fitAssumptions(as []string) []string {
	const limit = 64
	if len(as) <= limit {
		return as
	}
	return slices.Clone(as[len(as)-limit:])
}

// clipText cuts s to at most n bytes on a rune boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// admission admits planned candidates to a budget, scans them for uniform content inside the loop (a
// uniform result gives its budget back) and caps the bytes scanned at four times the byte limit; the
// verdict of an identical run list is cached (C2).
type admission struct {
	b           *budget
	img         io.ReaderAt
	keepUniform bool
	scanCap     int64
	scanned     int64
	cache       map[[sha256.Size]byte]uniformVerdict
}

type uniformVerdict struct{ uniform bool }

func newAdmission(b *budget, img io.ReaderAt, keepUniform bool) *admission {
	scan := b.maxBytes
	if scan > (1<<63-1)/4 {
		scan = 1<<63 - 1
	} else {
		scan *= 4
	}
	return &admission{b: b, img: img, keepUniform: keepUniform, scanCap: scan, cache: map[[sha256.Size]byte]uniformVerdict{}}
}

func runsKey(runs []evidence.Run) [sha256.Size]byte {
	h := sha256.New()
	var buf [16]byte
	for _, r := range runs {
		binary.LittleEndian.PutUint64(buf[:8], uint64(r.Offset))
		binary.LittleEndian.PutUint64(buf[8:], uint64(r.Length))
		h.Write(buf[:])
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// release gives back what admit counted.
func (b *budget) release(size int64) {
	b.files--
	b.bytes -= size
}

// admit decides one candidate. stop is "" to go on, else the limit that ends admission (the candidate is
// then marked limit). A negative size is an internal error.
func (ad *admission) admit(ctx context.Context, c *PlannedCandidate) (stop string, err error) {
	switch r := ad.b.admit(c.Size); r {
	case "":
	case reasonInvalidSize:
		return "", fmt.Errorf("%w: candidate size %d (internal error)", errInvalidSize, c.Size)
	default:
		c.Skip, c.skipDetail = "limit", r+" reached"
		return r, nil
	}
	key := runsKey(c.Runs)
	v, hit := ad.cache[key]
	if !hit {
		if c.Size > ad.scanCap-ad.scanned {
			ad.b.release(c.Size)
			c.Skip, c.skipDetail = "limit", "max-bytes: the content scan would read more than four times the byte limit"
			return "max-bytes", nil
		}
		uniform, _, err := uniformScan(ctx, ad.img, c.Runs)
		if err != nil {
			ad.b.release(c.Size)
			return "", err
		}
		ad.scanned += c.Size
		v = uniformVerdict{uniform: uniform}
		ad.cache[key] = v
	}
	c.Content = "ok"
	if v.uniform {
		c.Content = "uniform"
		if !ad.keepUniform {
			ad.b.release(c.Size)
			c.Skip, c.skipDetail = "uniform", "every byte has the same value"
		}
	}
	return "", nil
}

// admit runs the budget admission over the remaining candidates in plan order and assigns ordinals.
func (p *planner) admit() error {
	ad := newAdmission(p.budget, p.s.Image, p.o.KeepUniform)
	stopped := ""
	notDone := 0
	for i := range p.plan.Items {
		for j := range p.plan.Items[i].Candidates {
			c := &p.plan.Items[i].Candidates[j]
			if c.Skip != "" {
				continue
			}
			if stopped != "" {
				c.Skip, c.skipDetail = "limit", stopped+" reached"
				notDone++
				continue
			}
			stop, err := ad.admit(p.ctx, c)
			if err != nil {
				return err
			}
			if c.Skip == "uniform" {
				p.skipped("uniform")
			}
			if stop != "" {
				stopped = stop
				notDone++
			}
		}
	}
	if stopped != "" {
		p.plan.NotProcessed += notDone
		p.plan.SkippedBy["limit"] += notDone
		if p.plan.LimitReached == "" {
			p.plan.LimitReached = stopped
		}
		p.plan.limits = append(p.plan.limits, PlanSkip{Reason: "limit", Detail: fmt.Sprintf("%s: %d candidates were not processed", stopped, notDone)})
	}
	ord := 0
	for i := range p.plan.Items {
		for j := range p.plan.Items[i].Candidates {
			c := &p.plan.Items[i].Candidates[j]
			if c.Content == "uniform" {
				p.plan.uniform++
			}
			switch c.Skip {
			case "uniform", "limit":
			case "":
				ord++
				c.Ordinal = ord
				p.plan.admitted += c.Size
			}
		}
	}
	// Candidate skips join the entry's list in candidate order (the not-selected ones are listed but
	// never audited as warnings).
	for i := range p.plan.Items {
		it := &p.plan.Items[i]
		for _, c := range it.Candidates {
			if c.Skip != "" {
				it.Skips = append(it.Skips, PlanSkip{Path: it.Path, ID: it.ID, Reason: c.Skip, Detail: c.skipDetail})
			}
		}
	}
	return nil
}

// cleanText makes reader-supplied text recordable: invalid UTF-8 and NUL become U+FFFD, and it is cut to
// n bytes on a rune boundary.
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(s, repl)
	if strings.ContainsRune(s, 0) {
		s = strings.ReplaceAll(s, nulStr, repl)
	}
	return clipText(s, n)
}

func cleanList(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = cleanText(s, 256)
	}
	return out
}

var (
	repl   = string(rune(0xFFFD))
	nulStr = string(rune(0))
)
