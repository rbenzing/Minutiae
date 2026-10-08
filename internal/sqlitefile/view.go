package sqlitefile

import (
	"fmt"
)

// BTreeKind says what a b-tree holds.
type BTreeKind uint8

// The two kinds of b-tree.
const (
	TableTree BTreeKind = iota
	IndexTree
)

// DBStatus is the state of a database and its companions, for the checklist
// that asks what the examiner is looking at.
type DBStatus struct {
	Info    Info
	WAL     *WALInfo
	Journal *JournalInfo
}

// Status composes the header information with the state of the write-ahead
// log and the rollback journal; both are nil while no companion is attached.
func (d *DB) Status() DBStatus {
	s := DBStatus{Info: d.Info()}
	if w := d.WAL(); w != nil {
		cp := w.Info
		cp.Generations = append([]WALGeneration(nil), w.Info.Generations...)
		s.WAL = &cp
	}
	if j := d.Journal(); j != nil {
		cp := j.Info
		cp.Segments = append([]JournalSegment(nil), j.Info.Segments...)
		s.Journal = &cp
	}
	return s
}

// View is an immutable snapshot of what a database presents as live: the page
// source stack (today the database file as found; the write-ahead log and
// journal overlays are layered on by later tasks), the warnings found so far
// a page cache and the schema once read. It is safe for concurrent use. The cache holds up to
// Limits.PageCacheBytes charged to the budget until Release.
type View struct {
	d     *DB
	e     *env
	info  Info
	src   pageSource
	cache *pageCache
	st    *counters
	warns *warnings
	addr  uint32
	sch   schemaCache
	lists listCache
}

// Live returns the live view of the database as it is now: Attach* calls made
// after it do not reach it. For a plain database (no companion files) the live
// state is the file as stored. The view's warnings start as the database's
// and grow as scans run.
func (d *DB) Live() *View { return d.view(true) }

// view builds a view of the attached companions: with rollback the applied
// hot journal is laid over the database file first, then the WAL over that
// (the engine's order); without it the database file as found plus the WAL.
func (d *DB) view(rollback bool) *View {
	a, jr, warns := d.companions()
	return d.viewOf(rollback, a, jr, warns)
}

// companions returns the attached files and the warnings of the database as one
// snapshot.
func (d *DB) companions() (a *attachedWAL, jr *attachedJournal, warns []Warning) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.wal, d.jr, d.warns.snapshot()
}

// viewOf is view over the given snapshot of the companions.
func (d *DB) viewOf(rollback bool, a *attachedWAL, jr *attachedJournal, warns []Warning) *View {
	st := &counters{}
	w := newWarnings(d.env.opts.Limits.MaxWarnings)
	for _, x := range warns {
		w.add(x)
	}
	v := &View{d: d, e: d.env, info: d.Info(), st: st, warns: w}
	v.src = dbSource{d}
	rolled := rollback && jr != nil && jr.scan.Info.Applied
	if rolled {
		v.applyJournal(jr)
	}
	if a != nil {
		v.applyWAL(a)
		if rolled && a.scan.Info.UsedByLive {
			v.warns.add(Warning{Code: WarnJournalAndWAL, File: FileJournal, Msg: "a hot journal and a WAL are both present: the rollback is applied first and the WAL's committed frames over it, as the engine does"})
		}
	}
	// A hot journal the library does not apply (a differing page size, a cap)
	// leaves an interrupted transaction in the file: that is not the live state.
	// The journal-hot warning is already on the view.
	if rollback && jr != nil && jr.scan.Info.Hot && !jr.scan.Info.Applied {
		if r := jr.scan.Info.NotAppliedReason; r == "page-size-mismatch" || r == "limit-reached" {
			v.src = refusedSource{&LiveUnavailableError{File: FileJournal, Reason: r}}
		}
	}
	v.cache = newPageCache(d.env, v.src, d.info.PageSize, st)
	v.addr = v.addressable()
	return v
}

// addressable computes the highest page number any source can supply: the
// pages the header (or the file) declares, clamped to the pages that really
// exist and to Limits.MaxPages. Everything sized by a page count (bitsets,
// caches) uses this, never the declared count.
func (v *View) addressable() uint32 {
	phys := int64(v.info.FilePages)
	n := min(int64(v.info.PageCount), phys, v.e.opts.Limits.MaxPages)
	if n < int64(v.info.PageCount) {
		v.warns.add(Warning{
			Code: WarnPageCountClamped,
			Msg:  fmt.Sprintf("the header counts %d pages; %d can be read (the file holds %d, the limit is %d)", v.info.PageCount, n, v.info.FilePages, v.e.opts.Limits.MaxPages),
		})
	}
	return uint32(max(n, 0))
}

// Info returns the header information of the view. Once the schema was read,
// EngineRefuses also lists the reasons found in it (Schema.EngineRefuses).
func (v *View) Info() Info {
	i := v.info
	i.EngineRefuses = append([]string(nil), v.info.EngineRefuses...)
	v.sch.mu.Lock()
	if v.sch.schema != nil {
		i.EngineRefuses = append(i.EngineRefuses, v.sch.schema.EngineRefuses...)
	}
	v.sch.mu.Unlock()
	return i
}

// Addressable is the highest page number any source of the view can supply,
// clamped to the pages that exist: the header's page count is an upper bound
// and never sizes an allocation.
func (v *View) Addressable() uint32 { return v.addr }

// Warnings returns the anomalies found so far, in the order first seen. The
// list grows as scans run.
func (v *View) Warnings() []Warning { return v.warns.snapshot() }

// Stats returns the work counters of the view.
func (v *View) Stats() Stats { return v.st.snapshot() }

// Release drops the page cache and gives its budget charge back. It is
// optional (the view stays usable and refills its cache) and exists so a
// caller can account for the budget exactly once it is done with a view.
func (v *View) Release() {
	v.cache.clear()
	v.sch.release(v.e.budget)
	v.lists.release(v.e.budget)
}

// ReadPage returns a copy of page pgno as the view presents it: for a page
// number outside 1..Addressable it is ErrPageUnavailable. The page is as long
// as the bytes present (a trailing partial page of a truncated file is
// shorter than a page).
func (v *View) ReadPage(pgno uint32) (pg Page, err error) {
	defer guard(&err)
	if pgno == 0 || pgno > v.addr {
		return Page{}, fmt.Errorf("%w: page %d is outside 1..%d", ErrPageUnavailable, pgno, v.addr)
	}
	data, loc, err := v.cache.read(pgno)
	if err != nil {
		return Page{}, err
	}
	return Page{Number: pgno, Data: append([]byte(nil), data...), Loc: loc}, nil
}

// warn records a warning about page pgno of the database file.
func (v *View) warn(code string, pgno uint32, format string, a ...any) {
	v.warns.add(Warning{Code: code, File: sourceFile(v.src, pgno), Page: pgno, Msg: fmt.Sprintf(format, a...)})
}
