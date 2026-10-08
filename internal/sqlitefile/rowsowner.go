package sqlitefile

import "fmt"

// The owner of a page when an image of it was written (ruling I-1). A page that
// belongs to table t today may have belonged to another table when the image was
// written, so BasisSchema needs the owner of the page in the image's own as-of
// state (its snapshot) to be the same object as today's. When that state's
// schema cannot be read, or names another owner, the identity falls back to the
// fit alone, the relation to live is unknown and the row carries owner-changed.

// maxSnapOwnerStates bounds the as-of views one Rows call builds (one per
// distinct state: WAL generation and slot, the database under the WAL, the
// rolled-back file, the rollback state of a journal).
const maxSnapOwnerStates = 64

// snapState is the schema and layout of one as-of state.
type snapState struct {
	v   *View
	sch *Schema
	lay *Layout
	ok  bool
}

// snapKey names the as-of state of an image. Images of the live state (freelist,
// orphan, beyond-end, live pages) have none: their owner is today's.
func snapKey(img PageImage) (string, bool) {
	switch img.Origin {
	case OriginWALSuperseded, OriginWALUncommitted, OriginWALStale, OriginWALUnverified:
		if img.WAL == nil {
			return "", false
		}
		return fmt.Sprintf("w%d:%d", img.WAL.Generation, img.WAL.Frame), true
	case OriginDBUnderWAL:
		return "dbw", true
	case OriginDBRolledBack:
		return "rb", true
	case OriginJournalBefore:
		return "jb", true
	}
	return "", false
}

// snapStateOf builds (once per state) the view of the image's as-of state and
// reads its schema and layout.
func (rp *rowPass) snapStateOf(img PageImage, key string) *snapState {
	if st, ok := rp.snaps[key]; ok {
		return st
	}
	st := &snapState{}
	if len(rp.snaps) >= maxSnapOwnerStates {
		rp.limitHit("snapshot-owner-views", fmt.Sprintf("more than %d distinct as-of states hold images; the owner of the pages of the rest is not proven", maxSnapOwnerStates))
		return st
	}
	rp.snaps[key] = st
	h := rp.h
	v := &View{
		d: h.d, e: h.d.env, info: h.live.info, src: &snapSource{img: img, quiet: true}, st: &counters{},
		warns: newWarnings(h.d.env.opts.Limits.MaxWarnings), addr: h.live.addr,
	}
	v.cache = newPageCache(h.d.env, v.src, h.d.info.PageSize, v.st)
	st.v = v
	sch, err := v.Schema(rp.ctx)
	if err != nil || sch.Skipped > 0 {
		return st
	}
	lay, err := v.Layout(rp.ctx)
	if err != nil || lay.SchemaIncomplete {
		return st
	}
	st.sch, st.lay, st.ok = sch, lay, true
	return st
}

// objectName returns the name and type of schema object number o (1 is the
// schema table itself).
func objectName(s *Schema, o uint32) (name, typ string, ok bool) {
	if o == 1 {
		return "sqlite_schema", "table", true
	}
	k := int(o) - 2
	// k < 0 only for o == 0, which no caller passes: asOfOwner returns before
	// this on Owner 0 and ownerOf reports ok only for a non-zero owner (the
	// guard is defensive; mutating it changes no behaviour, review I-5).
	if k < 0 || k >= len(s.Objects) {
		return "", "", false
	}
	return s.Objects[k].Name, s.Objects[k].Type, true
}

// asOfOwner is the schema object that owned page img.Number in the image's own
// as-of state (ruling C44): its name and type, the object itself in the as-of
// schema (nil for the schema table), its number there. known is false when it
// cannot be proven. An image of the live state (freelist, orphan, ...) has no
// as-of state of its own: its owner is today's, which the caller handles.
type asOf struct {
	name, typ string
	obj       *SchemaObject
	num       uint32
}

func (rp *rowPass) asOfOwner(img PageImage) (a asOf, known bool) {
	if _, ok := snapKey(img); !ok {
		return asOf{}, false
	}
	if img.Origin == OriginDBUnderWAL {
		img.Origin = OriginDBRolledBack // the image is a page of the database file: its state is the file as found
	}
	img = rp.atEndOfTransaction(img)
	key, _ := snapKey(img)
	st := rp.snapStateOf(img, key)
	// Number == 0 is defensive: the history never yields an image of page 0 and
	// Class[0] is no b-tree class, so the class test below refuses it anyway
	// (review I-5, equivalent mutant).
	if !st.ok || img.Number == 0 || img.Number > st.lay.Addressable {
		return asOf{}, false
	}
	if c := st.lay.Class[img.Number]; c != ClassBTreeInterior && c != ClassBTreeLeaf {
		return asOf{}, false
	}
	o := st.lay.Owner[img.Number]
	if o == 0 {
		return asOf{}, false
	}
	name, typ, ok := objectName(st.sch, o)
	if !ok {
		return asOf{}, false
	}
	a = asOf{name: name, typ: typ, num: o}
	if o != 1 {
		a.obj = &st.sch.Objects[int(o)-2]
	}
	return a, true
}

// sameOwnerAtWrite reports whether page img.Number belonged to the same schema
// object (by name and type) in the image's own state as it does today (live
// owner liveOwner). Anything that cannot be proven is false.
func (rp *rowPass) sameOwnerAtWrite(img PageImage, liveOwner uint32) bool {
	if _, ok := snapKey(img); !ok {
		return true
	}
	a, ok := rp.asOfOwner(img)
	ln, ltp, ok2 := objectName(rp.sch, liveOwner)
	return ok && ok2 && a.name == ln && a.typ == ltp
}

// releaseSnaps frees the as-of views.
func (rp *rowPass) releaseSnaps() {
	for _, st := range rp.snaps {
		if st.v != nil {
			st.v.Release()
		}
	}
}

// atEndOfTransaction moves a WAL image to the end of the transaction that wrote
// it: the state between the frames of one transaction is not a database, so the
// owner is read from the state after the commit frame (or, for frames no commit
// closes, after the last frame of their generation).
func (rp *rowPass) atEndOfTransaction(img PageImage) PageImage {
	// h.a (the WAL analysis) is nil exactly when no WAL is attached, and an
	// image carries a WAL position only when one is: the second test is
	// defensive (review I-5, equivalent mutant).
	if img.WAL == nil || rp.h.a == nil {
		return img
	}
	frames := rp.h.a.scan.Frames
	end := img.WAL.Frame
	for i := int(img.WAL.Frame) - 1; i >= 0 && i < len(frames); i++ {
		f := frames[i]
		// The file keeps stale frames of an older generation past the end of the
		// current one, so a frame of another generation can follow. resolveWAL
		// keys pages by (generation, page, slot), so moving end onto such a slot
		// would change nothing: the break states the intent (review I-5, the
		// mutant is equivalent; TestAsOfTransactionEndStaysInItsGeneration pins
		// the behaviour).
		if f.Generation != img.WAL.Generation {
			break
		}
		end = f.Slot
		if f.DBSize != 0 {
			break
		}
	}
	if end == img.WAL.Frame {
		return img
	}
	w := *img.WAL
	w.Frame = end
	img.WAL = &w
	return img
}
