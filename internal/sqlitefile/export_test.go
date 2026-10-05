package sqlitefile

import (
	"context"
	"io"
	"sync"
)

// Test-only access to internals. Panic injection reaches library code only
// through the instance-scoped env.hook set here (there is no package-level
// variable).

// ResolveLimits exposes Limits.withDefaults.
func ResolveLimits(l Limits) Limits { return l.withDefaults() }

// EnvBudget returns the budget an instance built from o would use.
func EnvBudget(o Options) Budget { return newEnv(o, nil).budget }

// EnvLimits returns the resolved limits of an instance built from o.
func EnvLimits(o Options) Limits { return newEnv(o, nil).opts.Limits }

// KnownWarningCodes returns the codes the collector accepts.
func KnownWarningCodes() []string {
	out := make([]string, 0, len(knownWarningCodes))
	for c := range knownWarningCodes {
		out = append(out, c)
	}
	return out
}

// TestWarnings wraps the unexported collector. Unknown codes panic, as the
// closed-set rule requires in tests (never in production).
type TestWarnings struct{ w *warnings }

// NewTestWarnings builds a strict collector with the given cap (0: default).
func NewTestWarnings(maxWarnings int) *TestWarnings {
	w := newWarnings(maxWarnings)
	w.unknown = func(code string) { panic("unknown warning code " + code) }
	return &TestWarnings{w}
}

// Add records a warning.
func (t *TestWarnings) Add(x Warning) { t.w.add(x) }

// Snapshot returns a copy of the recorded warnings.
func (t *TestWarnings) Snapshot() []Warning { return t.w.snapshot() }

// Probe is a stand-in for a library instance: it carries an env whose hook
// the test sets, and runs guarded.
type Probe struct{ env *env }

// NewProbeWithHook returns an instance whose hook runs at every site.
func NewProbeWithHook(hook func(site string)) *Probe {
	return &Probe{env: newEnv(Options{}, hook)}
}

// Run calls the hook for site inside the guard, as every exported method does.
func (p *Probe) Run(site string) (err error) {
	defer guard(&err)
	p.env.at(site)
	return nil
}

// GuardedCall runs fn under guard.
func GuardedCall(fn func() error) (err error) {
	defer guard(&err)
	return fn()
}

// RawPage reads page pgno of the database file as found.
func (d *DB) RawPage(pgno uint32) ([]byte, error) { return d.readRawPage(pgno) }

// OpenWithHook is Open with a panic-injection hook (called at every site).
func OpenWithHook(db io.ReaderAt, size int64, opts Options, hook func(site string)) (*DB, error) {
	return openWith(db, size, opts, hook)
}

// ResolveLimitsReport exposes Limits.resolve: the limits in effect and one
// note per field clamped to its hard ceiling.
func ResolveLimitsReport(l Limits) (Limits, []string) { return l.resolve() }

// EffectiveLimits returns the limits the instance runs with.
func (d *DB) EffectiveLimits() Limits { return d.env.opts.Limits }

// TestLedger wraps a call-scoped budget ledger.
type TestLedger struct{ l *ledger }

// Alloc charges n bytes to the call.
func (t *TestLedger) Alloc(n int64) error { return t.l.alloc(n) }

// Free returns n bytes the call charged.
func (t *TestLedger) Free(n int64) { t.l.free(n) }

// LedgerCall runs fn as an exported method would: with a ledger on budget b
// and the guard deferred.
func LedgerCall(b Budget, fn func(*TestLedger) error) (err error) {
	e := newEnv(Options{Budget: b}, nil)
	l := e.newLedger()
	defer l.guard(&err)
	return fn(&TestLedger{l})
}

// ---- Task 3: sources, cache, payload, records ----

// FakeSource is an in-memory page source. It records every read.
type FakeSource struct {
	Pages map[uint32][]byte
	Locs  map[uint32]PageLoc
	Errs  map[uint32]error
	Reads []uint32
	mu    sync.Mutex
}

// NewFakeSource returns an empty source.
func NewFakeSource() *FakeSource {
	return &FakeSource{Pages: map[uint32][]byte{}, Locs: map[uint32]PageLoc{}, Errs: map[uint32]error{}}
}

func (f *FakeSource) has(pgno uint32) bool { _, ok := f.Pages[pgno]; return ok }

func (f *FakeSource) read(pgno uint32) ([]byte, PageLoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reads = append(f.Reads, pgno)
	if err := f.Errs[pgno]; err != nil {
		return nil, PageLoc{}, err
	}
	d, ok := f.Pages[pgno]
	if !ok {
		return nil, PageLoc{}, ErrPageUnavailable
	}
	loc, ok := f.Locs[pgno]
	if !ok {
		loc = PageLoc{File: FileDB, Offset: int64(pgno-1) * int64(len(d))}
	}
	return d, loc, nil
}

// TestEnv is an instance environment for the internal pieces.
type TestEnv struct{ e *env }

// NewTestEnv resolves o as Open does.
func NewTestEnv(o Options) *TestEnv { return &TestEnv{newEnv(o, nil)} }

// Ledger returns a new call-scoped budget ledger.
func (t *TestEnv) Ledger() *TestLedger { return &TestLedger{t.e.newLedger()} }

// Visited is a visited-set under test.
type Visited interface {
	Mark(pgno uint32) bool
	Release(l *TestLedger)
}

type visitedAdapter struct{ v visitor }

func (a visitedAdapter) Mark(p uint32) bool { return a.v.mark(p) }

// Release gives the set's charge back.
func (a visitedAdapter) Release(l *TestLedger) {
	switch s := a.v.(type) {
	case *pageSet:
		s.release(l.l)
	case *mapVisitor:
		s.release(l.l)
	}
}

// NewPageSetVisited returns a bitset visitor for pages 1..addressable.
func (t *TestEnv) NewPageSetVisited(l *TestLedger, addressable uint32) (Visited, error) {
	s, err := newPageSet(l.l, addressable)
	if err != nil {
		return nil, err
	}
	return visitedAdapter{s}, nil
}

// NewMapVisited returns a bounded map visitor.
func (t *TestEnv) NewMapVisited(l *TestLedger, n int) (Visited, error) {
	m, err := newMapVisitor(l.l, n)
	if err != nil {
		return nil, err
	}
	return visitedAdapter{m}, nil
}

// TestPayload wraps a cell's lazily read payload.
type TestPayload struct{ p *payload }

// NewTestPayload prepares the payload of cell c over src.
func (t *TestEnv) NewTestPayload(src *FakeSource, l *TestLedger, vis Visited, usable int, c Cell) *TestPayload {
	return &TestPayload{newPayload(src, l.l, vis.(visitedAdapter).v, usable, t.e.overflowCap(usable), c)}
}

// ReadAt reads up to len(dst) payload bytes at off.
func (p *TestPayload) ReadAt(dst []byte, off int64) (int, error) { return p.p.readAt(dst, off) }

// Chain lists the overflow pages followed so far.
func (p *TestPayload) Chain() []ChainStep {
	var out []ChainStep
	for _, s := range p.p.provenance() {
		out = append(out, ChainStep(s))
	}
	return out
}

// ChainStep is an exported chainStep.
type ChainStep struct {
	Page uint32
	At   PageLoc
}

// Damaged reports why the chain could not be followed, if it could not.
func (p *TestPayload) Damaged() (string, uint32, bool) { return p.p.damaged() }

// Release gives the step charges back.
func (p *TestPayload) Release() { p.p.release() }

// SetContext makes the payload poll ctx at each followed page.
func (p *TestPayload) SetContext(ctx context.Context) { p.p.ctx = ctx }

// ReadRecord decodes the payload's record lazily; held is what stays charged
// for the result. Warnings go to the returned snapshot function.
func (t *TestEnv) ReadRecord(l *TestLedger, p *TestPayload, enc Encoding, want func(int) bool) (rec Record, held int64, warns []Warning, err error) {
	w := newWarnings(0)
	w.unknown = func(code string) { panic("unknown warning code " + code) }
	defer func() { warns = w.snapshot() }()
	rec, held, err = t.e.readRecord(l.l, w, cellCtx{File: FileDB, Page: 7, Offset: 99}, p.p, enc, want)
	return
}

// DecodeRecovered is DecodeRecord for recovered values: over-cap values are
// clipped, not omitted.
func DecodeRecovered(b []byte, enc Encoding, lim Limits) (Record, error) {
	return decodeRecord(b, enc, lim.withDefaults(), true)
}

// TestCache is a page cache under test.
type TestCache struct{ c *pageCache }

// NewTestCache returns a cache over src for pages of pageSize bytes.
func (t *TestEnv) NewTestCache(src *FakeSource, pageSize int) *TestCache {
	return &TestCache{newPageCache(t.e, src, pageSize, nil)}
}

// Read serves a page.
func (c *TestCache) Read(pgno uint32) ([]byte, PageLoc, error) { return c.c.read(pgno) }

// Stats returns the counters.
func (c *TestCache) Stats() Stats { return c.c.st.snapshot() }

// Clear drops every page and gives the charges back.
func (c *TestCache) Clear() { c.c.clear() }

// DBCache returns a cache over the database file of d.
func (d *DB) DBCache() *TestCache {
	e := d.env
	return &TestCache{newPageCache(e, dbSource{d}, d.info.PageSize, nil)}
}

// PayloadOf returns the lazy payload of cell c of a database, read through a
// cache over the file, with a bounded visitor.
func (d *DB) PayloadOf(l *TestLedger, c Cell) (*TestPayload, error) {
	cache := newPageCache(d.env, dbSource{d}, d.info.PageSize, nil)
	vis, err := newMapVisitor(l.l, 4096)
	if err != nil {
		return nil, err
	}
	return &TestPayload{newPayload(cache, l.l, vis, d.info.UsableSize, d.env.overflowCap(d.info.UsableSize), c)}, nil
}

// Call runs fn as an exported method would: with a ledger on the
// environment's budget and the guard deferred (a panic or an error gives
// every charge of the call back).
func (t *TestEnv) Call(fn func(l *TestLedger) error) (err error) {
	l := t.e.newLedger()
	defer l.guard(&err)
	return fn(&TestLedger{l})
}

// ---- Task 4: view, scan, lookup, locations ----

// LocFor builds the Loc of cell c at pointer index idx of the page whose image
// lies at at, from the overflow pages (page, location) it followed.
func LocFor(at PageLoc, pgno uint32, idx int, c Cell, steps []ChainStep, maxOverflow int) Loc {
	var s []chainStep
	for _, x := range steps {
		s = append(s, chainStep(x))
	}
	return newLoc(at, pgno, idx, c, s, maxOverflow)
}

// PtrmapPageno exposes ptrmapPageno.
func PtrmapPageno(pageSize, reserved int, pgno uint32) uint32 {
	return ptrmapPageno(pageSize, reserved, pgno)
}

// ---- Task 5 step 0: the shared overflow-chain cap ----

// MaxMapVisited is the hard bound of a mapVisitor.
const MaxMapVisited = maxMapVisited

// OverflowPageCap exposes overflowPageCap.
func OverflowPageCap(lim Limits, usable int) int64 {
	return overflowPageCap(lim.withDefaults(), usable)
}

// ---- Task 5: schema and CREATE parser ----

// ParseCreateTable parses a CREATE TABLE statement with the default column cap.
func ParseCreateTable(sql string) (def TableDef, virtual bool, steps int) {
	return parseTableSQL(sql, DefaultLimits().MaxColumns)
}

// ParseCreateTableCols is ParseCreateTable with an explicit column cap.
func ParseCreateTableCols(sql string, maxCols int) (def TableDef, virtual bool, steps int) {
	return parseTableSQL(sql, maxCols)
}

// ParseCreateIndex parses a CREATE INDEX statement.
func ParseCreateIndex(sql string) (def IndexDef, steps int) {
	return parseIndexSQL(sql, DefaultLimits().MaxColumns)
}

// ListCharges returns the budget charged to the view's cached freelist and layout.
func ListCharges(v *View) (freelist, layout int64) {
	v.lists.mu.Lock()
	defer v.lists.mu.Unlock()
	return v.lists.flCharge, v.lists.layCharge
}
