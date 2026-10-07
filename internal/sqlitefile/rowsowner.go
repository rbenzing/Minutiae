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
		d: h.d, e: h.d.env, info: h.d.Info(), src: &snapSource{img: img, quiet: true}, st: &counters{},
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
	if k < 0 || k >= len(s.Objects) {
		return "", "", false
	}
	return s.Objects[k].Name, s.Objects[k].Type, true
}

// sameOwnerAtWrite reports whether page img.Number belonged to the same schema
// object (by name and type) in the image's own state as it does today (live
// owner liveOwner). Anything that cannot be proven is false.
func (rp *rowPass) sameOwnerAtWrite(img PageImage, liveOwner uint32) bool {
	key, ok := snapKey(img)
	if !ok {
		return true
	}
	if img.Origin == OriginDBUnderWAL {
		img.Origin = OriginDBRolledBack // the image is a page of the database file: its state is the file as found
	}
	st := rp.snapStateOf(img, key)
	if !st.ok || img.Number == 0 || img.Number > st.lay.Addressable {
		return false
	}
	if c := st.lay.Class[img.Number]; c != ClassBTreeInterior && c != ClassBTreeLeaf {
		return false
	}
	o := st.lay.Owner[img.Number]
	if o == 0 {
		return false
	}
	sn, stp, ok1 := objectName(st.sch, o)
	ln, ltp, ok2 := objectName(rp.sch, liveOwner)
	return ok1 && ok2 && sn == ln && stp == ltp
}

// releaseSnaps frees the as-of views.
func (rp *rowPass) releaseSnaps() {
	for _, st := range rp.snaps {
		if st.v != nil {
			st.v.Release()
		}
	}
}
