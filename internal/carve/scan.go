package carve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
)

// Scan limits.
const (
	// DefaultMaxHitsPerType is the accepted-hit cap of one type; past it the
	// type stops matching (Stats.TypeCapped).
	DefaultMaxHitsPerType = 1_000_000
	// DefaultValidateFactor and DefaultValidateSlack give the default validator
	// read budget: factor x scope bytes + slack, never above MaxValidateBudget.
	DefaultValidateFactor int64 = 8
	DefaultValidateSlack  int64 = 64 << 20

	// sectorSize is the granularity at which an unreadable range is narrowed.
	sectorSize = 512
	// footerChunk is the read size of a footer search.
	footerChunk = 64 << 10
	// maxDepth is the deepest pass: 0 top level, 1 nested hits, 2 counting only.
	maxDepth = 2
)

// ErrBadRun: a run is negative or leaves the int64 range.
var ErrBadRun = errors.New("carve: bad run")

// Run is a contiguous byte range of the reader that no hit may leave.
type Run struct{ Offset, Length int64 }

// Options tune a scan.
type Options struct {
	// Align is the header unit relative to each run's start; <= 1 tests every byte.
	Align int64
	// Nested scans the inside of every top-level validated hit at every byte.
	Nested bool
	// TypeFilter maps a family to the refined names kept (from Set.Select).
	TypeFilter map[string][]string
	// MaxHitsPerType caps the accepted hits of one type; 0 is DefaultMaxHitsPerType.
	MaxHitsPerType int
	// ValidateBudget caps the bytes validators may read; 0 is the default
	// (DefaultValidateFactor x scope bytes + DefaultValidateSlack), and no value
	// exceeds MaxValidateBudget.
	ValidateBudget int64
	// OnReadError is called once for each newly found unreadable range; nil
	// ends the scan with the read error, a non-nil return ends it with that error.
	OnReadError func(off, n int64, err error) error
	// Progress receives the bytes scanned so far, after each chunk.
	Progress func(scanned int64)
}

// Hit is one accepted object, in reader coordinates.
type Hit struct {
	Family, Type, Ext, MIME string
	Offset, Length          int64
	Run                     int
	Outcome                 Outcome
	Method                  string // carve-validated | carve-footer | carve-bounded
	Confidence              int
	Basis, Assumptions      []string
	NestedIn                int64 // -1 for a top-level hit
	Filtered                bool  // the refined type is not selected
}

// Stats counts what a scan did; a count of zero is a fact only for the bytes
// Scanned reports.
type Stats struct {
	Scanned, ValidateBytes, OutsideReads, UnreadableBytes int64
	Accepted                                              map[Outcome]int
	Rejected                                              map[string]int
	NestedDepthSkipped                                    int
	TypeCapped                                            []string
	BudgetExhausted                                       bool

	maxReported int // peak length of the counted-unreadable list (a memory bound)
	budgetLimit int64
}

// PanicError is a panic recovered inside a scan: a defect, never a crash.
type PanicError struct{ Value any }

func (e *PanicError) Error() string { return fmt.Sprintf("carve: panic: %v", e.Value) }

// Confidence is the confidence an outcome earns before provenance caps.
func Confidence(o Outcome) int {
	switch o {
	case Validated:
		return 70
	case Footer:
		return 50
	case Bounded:
		return 25
	}
	return 0
}

// MethodOf is the recovery method of an outcome ("" for rejected).
func MethodOf(o Outcome) string {
	switch o {
	case Validated:
		return "carve-validated"
	case Footer:
		return "carve-footer"
	case Bounded:
		return "carve-bounded"
	}
	return ""
}

// resolveBudget is the validator read budget: an explicit request, else the
// default for the scope, never above MaxValidateBudget whatever the scope size.
func resolveBudget(requested, scopeBytes int64) int64 {
	if requested != 0 {
		return min(requested, MaxValidateBudget)
	}
	if scopeBytes > (MaxValidateBudget-DefaultValidateSlack)/DefaultValidateFactor {
		return MaxValidateBudget
	}
	return scopeBytes*DefaultValidateFactor + DefaultValidateSlack
}

// Scan carves the runs of r for the objects of set and emits each accepted
// hit. A hit never leaves its run. Nothing is decompressed and nothing is
// allocated from a declared size. A panic comes back as *PanicError.
func Scan(ctx context.Context, r io.ReaderAt, runs []Run, set *Set, o Options, emit func(Hit) error) (Stats, error) {
	if err := set.Check(); err != nil {
		return Stats{Accepted: map[Outcome]int{}, Rejected: map[string]int{}}, err
	}
	return scan(ctx, r, runs, set, o, builtinValidators(), emit)
}

type ctype struct {
	t        Type
	validate validatorFunc
	any      bool
	count    int
	capped   bool
}

// anchorGroup dispatches on the byte at one offset: table[b] lists the types
// whose smallest-offset header pattern accepts b there.
type anchorGroup struct {
	off   int
	table [256][]int
}

type rng struct{ lo, hi int64 }

type scanner struct {
	ctx    context.Context
	r      io.ReaderAt
	runs   []Run
	o      Options
	emit   func(Hit) error
	types  []ctype
	groups []anchorGroup
	// overlap is the longest header reach, so a chunk keeps the bytes a header
	// at its last candidate position needs.
	overlap int
	hasAny  bool
	maxHits int
	bud     *budget
	stats   Stats

	reported      []rng // unreadable ranges already counted, sorted and disjoint
	bufs          [maxDepth + 1][]byte
	fbuf          []byte
	runPos        int64
	runUnreadable int64
}

func newScanner(ctx context.Context, r io.ReaderAt, runs []Run, set *Set, o Options, v map[string]validatorFunc, emit func(Hit) error) (*scanner, error) {
	if len(set.Types) == 0 {
		return nil, fmt.Errorf("%w: the set has no types", ErrBadSignature)
	}
	var scope int64
	for _, ru := range runs {
		if ru.Offset < 0 || ru.Length < 0 || ru.Offset > math.MaxInt64-ru.Length {
			return nil, fmt.Errorf("%w: offset %d length %d", ErrBadRun, ru.Offset, ru.Length)
		}
		if scope > math.MaxInt64-ru.Length {
			scope = math.MaxInt64
		} else {
			scope += ru.Length
		}
	}
	s := &scanner{ctx: ctx, r: r, runs: runs, o: o, emit: emit, maxHits: o.MaxHitsPerType}
	if s.maxHits <= 0 {
		s.maxHits = DefaultMaxHitsPerType
	}
	s.bud = newBudget(resolveBudget(o.ValidateBudget, scope))
	s.stats = Stats{Accepted: map[Outcome]int{}, Rejected: map[string]int{}, budgetLimit: s.bud.limit}
	byOff := map[int]*anchorGroup{}
	for i := range set.Types {
		t := set.Types[i].clone()
		ct := ctype{t: t, any: t.Align == alignAny}
		if t.Validate != "" {
			fn, ok := v[t.Validate]
			if !ok {
				return nil, fmt.Errorf("%w: type %q: unknown validator %q", ErrBadSignature, t.Name, t.Validate)
			}
			ct.validate = fn
		}
		if len(t.Headers) == 0 {
			return nil, fmt.Errorf("%w: type %q has no headers", ErrBadSignature, t.Name)
		}
		anchor := t.Headers[0]
		for _, h := range t.Headers {
			s.overlap = max(s.overlap, h.Offset+len(h.Bytes))
			if h.Offset < anchor.Offset {
				anchor = h
			}
		}
		g := byOff[anchor.Offset]
		if g == nil {
			g = &anchorGroup{off: anchor.Offset}
			byOff[anchor.Offset] = g
		}
		for b := range 256 {
			if patternAccepts(anchor, 0, byte(b)) {
				g.table[b] = append(g.table[b], len(s.types))
			}
		}
		s.hasAny = s.hasAny || ct.any
		s.types = append(s.types, ct)
	}
	for _, g := range byOff {
		s.groups = append(s.groups, *g)
	}
	slices.SortFunc(s.groups, func(a, b anchorGroup) int { return a.off - b.off })
	return s, nil
}

// patternAccepts reports whether byte b equals pattern byte i under the mask.
func patternAccepts(p Pattern, i int, b byte) bool {
	m := byte(0xFF)
	if p.Mask != nil {
		m = p.Mask[i]
	}
	return b&m == p.Bytes[i]&m
}

func scan(ctx context.Context, r io.ReaderAt, runs []Run, set *Set, o Options, v map[string]validatorFunc, emit func(Hit) error) (st Stats, err error) {
	s, err := newScanner(ctx, r, runs, set, o, v, emit)
	if err != nil {
		return Stats{Accepted: map[Outcome]int{}, Rejected: map[string]int{}}, err
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = &PanicError{Value: rec}
		}
		s.stats.ValidateBytes = s.bud.spent
		s.stats.BudgetExhausted = s.bud.exhausted
		st = s.stats
	}()
	for ri, ru := range runs {
		if ru.Length == 0 {
			continue
		}
		s.runUnreadable, s.runPos, s.reported = 0, ru.Offset, s.reported[:0]
		p := &pass{s: s, ri: ri, lo: ru.Offset, hi: ru.Offset + ru.Length, base: ru.Offset, unit: 1, outer: -1}
		if o.Align > 1 {
			p.unit = o.Align
		}
		err := p.run()
		done := s.runPos - ru.Offset
		if err == nil {
			done = ru.Length
		}
		s.stats.Scanned += max(0, done-s.runUnreadable)
		if err != nil {
			return s.stats, err
		}
		if o.Progress != nil {
			o.Progress(s.stats.Scanned)
		}
	}
	return s.stats, nil
}

// reportBad counts the part of [lo, hi) not yet counted as unreadable and
// hands it to OnReadError, or fails the scan when there is none.
func (s *scanner) reportBad(lo, hi int64, cause error) error {
	i := sort.Search(len(s.reported), func(i int) bool { return s.reported[i].hi >= lo })
	var fresh []rng
	cur := lo
	for j := i; j < len(s.reported) && s.reported[j].lo <= hi && cur < hi; j++ {
		if s.reported[j].lo > cur {
			fresh = append(fresh, rng{cur, min(s.reported[j].lo, hi)})
		}
		cur = max(cur, s.reported[j].hi)
	}
	if cur < hi {
		fresh = append(fresh, rng{cur, hi})
	}
	// Merge [lo, hi) into the counted ranges.
	merged := rng{lo, hi}
	j := i
	for j < len(s.reported) && s.reported[j].lo <= merged.hi {
		merged.lo, merged.hi = min(merged.lo, s.reported[j].lo), max(merged.hi, s.reported[j].hi)
		j++
	}
	s.reported = slices.Replace(s.reported, i, j, merged)
	s.stats.maxReported = max(s.stats.maxReported, len(s.reported))
	for _, f := range fresh {
		n := f.hi - f.lo
		s.stats.UnreadableBytes += n
		s.runUnreadable += n
		if s.o.OnReadError == nil {
			return fmt.Errorf("carve: reading %d bytes at offset %d: %w", n, f.lo, cause)
		}
		if err := s.o.OnReadError(f.lo, n, cause); err != nil {
			return err
		}
	}
	return nil
}

// pruneReported drops the counted ranges that end at or before the top-level
// chunk start: the top-level scan only moves forward and the nested passes
// read ahead of it, so no later read can reach them again.
func (s *scanner) pruneReported(pos int64) {
	i := sort.Search(len(s.reported), func(i int) bool { return s.reported[i].hi > pos })
	if i > 0 {
		s.reported = append(s.reported[:0], s.reported[i:]...)
	}
}

// pass scans one span [lo, hi) of one run. Depth 0 is the run itself on the
// header grid; depth 1 the inside of a top-level validated hit at every byte;
// depth 2 the inside of a nested hit, which only counts what it skips.
type pass struct {
	s      *scanner
	depth  int
	ri     int
	lo, hi int64
	base   int64
	unit   int64
	outer  int64

	bufOff int64
	bufLen int
	owned  int64
	bad    []rng // unreadable ranges of the loaded chunk, sorted, disjoint
}

func (s *scanner) buffer(depth int) []byte {
	if s.bufs[depth] == nil {
		s.bufs[depth] = make([]byte, DefaultBufSize+s.overlap)
	}
	return s.bufs[depth]
}

func (p *pass) alignUp(pos int64) int64 {
	if p.unit <= 1 {
		return pos
	}
	rel := pos - p.base
	if r := rel % p.unit; r != 0 {
		if add := p.unit - r; pos <= math.MaxInt64-add {
			return pos + add
		}
		return p.hi // no further grid position inside the span
	}
	return pos
}

func (p *pass) step(pos int64) int64 {
	if p.unit > 1 && !p.s.hasAny {
		return p.alignUp(pos + 1)
	}
	return pos + 1
}

// load reads the chunk that starts at pos: DefaultBufSize owned positions plus
// the overlap their headers can reach.
func (p *pass) load(pos int64) error {
	n := int(min(p.hi-pos, int64(DefaultBufSize+p.s.overlap)))
	if p.depth == 0 {
		p.s.pruneReported(pos)
	}
	p.bufOff, p.bufLen, p.bad = pos, n, p.bad[:0]
	p.owned = min(pos+DefaultBufSize, p.hi)
	return p.readInto(p.s.buffer(p.depth)[:n], pos)
}

// readInto fills buf from off. A failed read keeps its readable prefix, and a
// reader that returns nothing is narrowed by halving down to one sector, so
// exactly the unreadable sectors are marked.
func (p *pass) readInto(buf []byte, off int64) error {
	n, err := p.s.r.ReadAt(buf, off)
	if n >= len(buf) {
		return nil
	}
	n = max(n, 0)
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	if errors.Is(err, io.EOF) {
		return p.markBad(off+int64(n), off+int64(len(buf)), err)
	}
	if n > 0 {
		return p.readInto(buf[n:], off+int64(n))
	}
	end := off + int64(len(buf))
	// Split on an absolute sector boundary, so a piece never mixes a readable
	// and an unreadable sector; a piece inside one sector is marked whole.
	mid := (off + int64(len(buf))/2) &^ (sectorSize - 1)
	if mid <= off {
		mid = (off &^ (sectorSize - 1)) + sectorSize
	}
	if mid >= end {
		return p.markBad(off, end, err)
	}
	k := int(mid - off)
	if e := p.readInto(buf[:k], off); e != nil {
		return e
	}
	return p.readInto(buf[k:], mid)
}

func (p *pass) markBad(lo, hi int64, cause error) error {
	if last := len(p.bad) - 1; last >= 0 && p.bad[last].hi == lo {
		p.bad[last].hi = hi
	} else {
		p.bad = append(p.bad, rng{lo, hi})
	}
	return p.s.reportBad(lo, hi, cause)
}

// badCovering returns the end of the unreadable range that contains pos, or 0.
func (p *pass) badCovering(pos int64) int64 {
	if len(p.bad) == 0 {
		return 0
	}
	i := sort.Search(len(p.bad), func(i int) bool { return p.bad[i].hi > pos })
	if i < len(p.bad) && p.bad[i].lo <= pos {
		return p.bad[i].hi
	}
	return 0
}

// badOverlaps reports whether [lo, hi) touches an unreadable range.
func (p *pass) badOverlaps(lo, hi int64) bool {
	if len(p.bad) == 0 {
		return false
	}
	i := sort.Search(len(p.bad), func(i int) bool { return p.bad[i].hi > lo })
	return i < len(p.bad) && p.bad[i].lo < hi
}

// limitAfter is the end of the readable stretch that holds pos, as far as the
// loaded chunk shows, capped by the span.
func (p *pass) limitAfter(pos int64) int64 {
	i := sort.Search(len(p.bad), func(i int) bool { return p.bad[i].lo > pos })
	if i < len(p.bad) {
		return min(p.hi, p.bad[i].lo)
	}
	return p.hi
}

func (p *pass) run() error {
	s := p.s
	pos := p.lo
	for pos < p.hi {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if p.depth == 0 {
			s.runPos = pos
		}
		if err := p.load(pos); err != nil {
			return err
		}
		if p.depth == 0 && s.o.Progress != nil {
			s.o.Progress(s.stats.Scanned + max(0, pos-p.lo-s.runUnreadable))
		}
		for pos < p.owned {
			if end := p.badCovering(pos); end > 0 {
				pos = p.alignUp(end)
				continue
			}
			next, err := p.try(pos)
			if err != nil {
				return err
			}
			pos = next
		}
	}
	if p.depth == 0 {
		s.runPos = p.hi
	}
	return nil
}

// headersMatch tests every header of the type at pos against the chunk.
func (p *pass) headersMatch(ct *ctype, pos int64) bool {
	rel := int(pos - p.bufOff)
	buf := p.s.bufs[p.depth]
	for _, h := range ct.t.Headers {
		at := rel + h.Offset
		if at+len(h.Bytes) > p.bufLen {
			return false
		}
		for i := range h.Bytes {
			if !patternAccepts(h, i, buf[at+i]) {
				return false
			}
		}
	}
	return !p.badOverlaps(pos, pos+int64(p.s.overlap))
}

// try tests the candidate position pos and returns where the scan goes next.
func (p *pass) try(pos int64) (int64, error) {
	s := p.s
	aligned := p.unit <= 1 || (pos-p.base)%p.unit == 0
	rel := int(pos - p.bufOff)
	next := p.step(pos)
	for gi := range s.groups {
		g := &s.groups[gi]
		at := rel + g.off
		if at >= p.bufLen {
			continue
		}
		for _, ti := range g.table[s.bufs[p.depth][at]] {
			ct := &s.types[ti]
			if ct.capped || (!aligned && !ct.any) || !p.headersMatch(ct, pos) {
				continue
			}
			end, err := p.candidate(ct, pos)
			if err != nil {
				return 0, err
			}
			if end > 0 { // validated: the object's inside is not scanned again
				return end, nil
			}
		}
	}
	return next, nil
}

func (p *pass) reject(reason string) {
	if p.depth < maxDepth {
		p.s.stats.Rejected[reason]++
	}
}

// candidate validates the header hit of ct at pos. It returns the hit's end
// when the object is validated (the scan resumes there), else 0.
func (p *pass) candidate(ct *ctype, pos int64) (int64, error) {
	s := p.s
	if p.depth < maxDepth && ct.count >= s.maxHits {
		ct.capped = true
		s.stats.TypeCapped = append(s.stats.TypeCapped, ct.t.Name)
		return 0, nil
	}
	limit := p.limitAfter(pos)
	winLen := min(ct.t.MaxLen, limit-pos)
	if winLen < ct.t.MinLen {
		p.reject("truncated")
		return 0, nil
	}
	if s.bud.exhausted {
		p.reject("validate-budget")
		return 0, nil
	}
	w := newWindow(s.ctx, s.r, pos, winLen, s.bud)
	var v Verdict
	if ct.validate != nil {
		v = ct.validate(w)
		if v.Outcome == Bounded && len(v.Assumptions) == 0 && v.Length == winLen {
			// A validator that can only bound the object to its window (gzip) leaves
			// the reason to the scanner, which knows the cap and the run.
			v.Assumptions = []string{p.boundedAssumption(ct, winLen, limit)}
		}
	} else {
		v = p.footerVerdict(ct, w, winLen, limit)
	}
	s.stats.OutsideReads += w.outsideReads()
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	accepted := v.Outcome == Validated || v.Outcome == Footer || v.Outcome == Bounded
	// A read error of the media rejects the hit whatever the validator says: it
	// may have swallowed the error. The interior is not skipped, so the next
	// chunk load finds and reports the unreadable range.
	if !accepted || w.readErr != nil {
		reason := v.Reason
		switch {
		case s.bud.exhausted:
			reason = "validate-budget"
		case w.readErr != nil:
			reason = "unreadable"
		case reason != "":
		case w.outsideReads() > 0:
			reason = "truncated"
		default:
			reason = "rejected"
		}
		p.reject(reason)
		return 0, nil
	}
	switch {
	case v.Length < 1 || v.Length > winLen:
		p.reject("bad-length")
		return 0, nil
	case v.Length < ct.t.MinLen:
		p.reject("too-short")
		return 0, nil
	}
	return p.accept(ct, pos, v)
}

// footerVerdict decides a type with no validator: the first footer inside the
// window ends the object, else the window does (bounded).
func (p *pass) footerVerdict(ct *ctype, w *Window, winLen, limit int64) Verdict {
	if len(ct.t.Footer) > 0 {
		hdrEnd := int64(0)
		for _, h := range ct.t.Headers {
			hdrEnd = max(hdrEnd, int64(h.Offset+len(h.Bytes)))
		}
		at, err := p.searchFooter(w, ct.t.Footer, min(hdrEnd, winLen))
		if err != nil {
			return Verdict{Outcome: Rejected}
		}
		if at >= 0 {
			return Verdict{Outcome: Footer, Length: at + int64(len(ct.t.Footer)), Basis: []string{"footer"}}
		}
	}
	return Verdict{Outcome: Bounded, Length: winLen, Assumptions: []string{p.boundedAssumption(ct, winLen, limit)}}
}

// boundedAssumption names why a bounded object ends at its window end: the
// type's maximum length, the end of the run, of the outer object, or of the
// readable range.
func (p *pass) boundedAssumption(ct *ctype, winLen, limit int64) string {
	switch {
	case winLen == ct.t.MaxLen:
		return "length-capped"
	case limit == p.hi && p.depth == 0:
		return "cut-at-run-end"
	case limit == p.hi:
		return "cut-at-outer-object-end"
	default:
		return "cut-at-unreadable-range"
	}
}

// searchFooter returns the offset of the first footer at or after from inside
// the window, or -1.
func (p *pass) searchFooter(w *Window, footer []byte, from int64) (int64, error) {
	s := p.s
	if s.fbuf == nil {
		s.fbuf = make([]byte, footerChunk+MaxPatternLen)
	}
	for off := from; off < w.Len(); {
		n := min(int64(len(s.fbuf)), w.Len()-off)
		if _, err := w.ReadAt(s.fbuf[:n], off); err != nil {
			return -1, err
		}
		if i := bytes.Index(s.fbuf[:n], footer); i >= 0 {
			return off + int64(i), nil
		}
		if off+n >= w.Len() || n <= int64(len(footer)) {
			break
		}
		off += n - int64(len(footer)) + 1
	}
	return -1, nil
}

// accept turns a verdict into a hit and, for a validated top-level or nested
// hit, scans its inside when nesting is on. It returns the hit's end for a
// validated verdict.
func (p *pass) accept(ct *ctype, pos int64, v Verdict) (int64, error) {
	s := p.s
	if p.depth == maxDepth { // inside a nested hit: counted, never reported
		s.stats.NestedDepthSkipped++
		if v.Outcome == Validated {
			return pos + v.Length, nil
		}
		return 0, nil
	}
	ct.count++
	s.stats.Accepted[v.Outcome]++
	h := Hit{
		Family: ct.t.Name, Type: ct.t.Name, Ext: ct.t.Ext, MIME: ct.t.MIME,
		Offset: pos, Length: v.Length, Run: p.ri, Outcome: v.Outcome,
		Method: MethodOf(v.Outcome), Confidence: Confidence(v.Outcome),
		Basis: slices.Clone(v.Basis), Assumptions: slices.Clone(v.Assumptions), NestedIn: -1,
	}
	if v.Type != "" {
		h.Type = v.Type
	}
	if v.Ext != "" {
		h.Ext = v.Ext
	}
	if p.depth == 1 {
		h.NestedIn = p.outer
		h.Assumptions = append(h.Assumptions, fmt.Sprintf("nested-in=%016x", p.outer))
	}
	if keep, ok := s.o.TypeFilter[h.Family]; ok && !slices.Contains(keep, h.Type) {
		h.Filtered = true
	}
	if err := s.emit(h); err != nil {
		return 0, err
	}
	if v.Outcome != Validated {
		return 0, nil
	}
	end := pos + v.Length
	if s.o.Nested {
		sub := &pass{s: s, depth: p.depth + 1, ri: p.ri, lo: pos + 1, hi: end, unit: 1, outer: pos}
		if err := sub.run(); err != nil {
			return 0, err
		}
	}
	return end, nil
}
