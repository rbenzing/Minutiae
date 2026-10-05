package sqlitefile

import (
	"context"
	"fmt"
)

// LookupRowid finds the row of the table tree rooted at root whose rowid is
// rowid. It binary-searches the cells of each page on the path (so it reads at
// most one page per level plus the overflow pages of the row it returns) and
// serves interior pages from the cache. The returned Row is the caller's.
//
// "Absent" (ok false, nil error) is answered only when the search path was
// clean. A path page that cannot be used, a cell the search needs that cannot be
// parsed, interior or leaf keys that contradict their neighbours or the key
// range their ancestors give them, a row that cannot be decoded: each ends in a
// *CorruptError (matching ErrCorrupt) instead, because the row may exist where
// the damage hides it. A miss at either end of a leaf that has a key bound also
// reads the adjacent leaf (at most one page per level) to prove that its rows
// respect the bound; a row that is found but lies outside its bounds is
// returned with KeyRangeViolation set, as the scan delivers it.
func (v *View) LookupRowid(ctx context.Context, root uint32, rowid int64) (row Row, ok bool, err error) {
	defer guard(&err)
	l := v.e.newLedger()
	defer l.guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Row{}, false, err
	}
	path, err := newMapVisitor(l, v.e.opts.Limits.MaxBTreeDepth+1)
	if err != nil {
		return Row{}, false, err
	}
	defer path.release(l)
	w := v.newWalker(ctx, l, path, TableTree)
	defer func() { l.free(w.held) }()
	var kb keyBounds
	pgno := root
	for depth := 1; ; depth++ {
		n, found, err := w.enter(pgno, depth)
		if err != nil {
			return Row{}, false, err
		}
		if !found {
			return Row{}, false, w.uncertain(pgno, "a page on the search path cannot be used")
		}
		if n.bad > 0 {
			w.damage("a page on the search path has cell pointers that cannot be followed")
		}
		if !n.h.Type.interior() {
			return w.lookupLeaf(n, rowid, kb)
		}
		idx, next, nkb, err := w.descend(n, rowid, kb)
		if err != nil {
			return Row{}, false, err
		}
		w.stack = append(w.stack, frame{n: n, k: idx, kb: kb})
		kb, pgno = nkb, next
	}
}

// damage records why the answer of this lookup cannot be "absent" (the first
// reason is kept).
func (w *walker) damage(reason string) {
	if w.dmg == "" {
		w.dmg = reason
	}
}

// uncertain is the error of a lookup that cannot tell: the row may exist behind
// the damage on page pgno.
func (w *walker) uncertain(pgno uint32, reason string) error {
	return &CorruptError{File: FileDB, Page: pgno, Reason: reason + ": the row may exist, the answer is uncertain"}
}

// absent is the answer of a lookup that found nothing: "absent" when the path
// was clean, else uncertain.
func (w *walker) absent(pgno uint32) (Row, bool, error) {
	if w.dmg != "" {
		return Row{}, false, w.uncertain(pgno, w.dmg)
	}
	return Row{}, false, nil
}

// probe parses the cell at pointer index i of n; ok is false (and the lookup is
// marked damaged) when it cannot be parsed.
func (w *walker) probe(n node, i int) (Cell, bool, error) {
	c, ok, err := w.cell(n, n.ptrs[i])
	if err != nil {
		return Cell{}, false, err
	}
	if !ok {
		w.damage("a cell the search needs cannot be parsed")
	}
	return c, ok, nil
}

// descend returns the child of interior page n that holds rowid (the left child
// of the first cell whose key is not below rowid, else the right-most child),
// its index and the key range it may hold. The cells on both sides of the
// chosen child are checked against rowid and against kb.
func (w *walker) descend(n node, rowid int64, kb keyBounds) (idx int, child uint32, nkb keyBounds, err error) {
	lo, hi := 0, len(n.ptrs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c, ok, err := w.probe(n, mid)
		if err != nil {
			return 0, 0, kb, err
		}
		if !ok { // an unreadable cell: probe on the left of it
			hi = mid
			continue
		}
		if c.Rowid >= rowid {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	idx = lo
	var prev, key int64
	havePrev, haveKey := false, false
	if idx > 0 {
		c, ok, err := w.probe(n, idx-1)
		if err != nil {
			return 0, 0, kb, err
		}
		if ok {
			prev, havePrev = c.Rowid, true
			if c.Rowid >= rowid {
				w.damage("interior keys are out of order")
			}
		}
	}
	child = n.h.RightChild
	if idx < len(n.ptrs) {
		c, ok, err := w.probe(n, idx)
		if err != nil {
			return 0, 0, kb, err
		}
		if !ok { // the child of an unreadable cell cannot be known
			return 0, 0, kb, w.uncertain(n.pgno, w.dmg)
		}
		key, haveKey = c.Rowid, true
		child = c.LeftChild
		if c.Rowid < rowid {
			w.damage("interior keys are out of order")
		}
	}
	if !kb.holds(rowid) {
		w.damage("an interior key contradicts the key range of its ancestors")
	}
	return idx, child, kb.child(n.pgno, prev, havePrev, key, haveKey), nil
}

// lookupLeaf binary-searches the cells of leaf n.
func (w *walker) lookupLeaf(n node, rowid int64, kb keyBounds) (Row, bool, error) {
	lo, hi := 0, len(n.ptrs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c, ok, err := w.probe(n, mid)
		if err != nil {
			return Row{}, false, err
		}
		switch {
		case !ok:
			hi = mid
		case c.Rowid == rowid:
			return w.found(n, n.ptrs[mid], c, kb)
		case c.Rowid > rowid:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	// A miss: the cells either side of the insertion point must agree, and the
	// leaf must respect its bounds at the ends.
	if lo > 0 {
		c, ok, err := w.probe(n, lo-1)
		if err != nil {
			return Row{}, false, err
		}
		if ok && (c.Rowid >= rowid || !kb.holds(c.Rowid)) {
			w.damage("a leaf key contradicts its neighbours or its key range")
		}
	}
	if lo < len(n.ptrs) {
		c, ok, err := w.probe(n, lo)
		if err != nil {
			return Row{}, false, err
		}
		if ok && (c.Rowid <= rowid || !kb.holds(c.Rowid)) {
			w.damage("a leaf key contradicts its neighbours or its key range")
		}
	}
	if w.dmg == "" && lo == len(n.ptrs) && kb.hasHi {
		if err := w.checkSibling(kb, true); err != nil {
			return Row{}, false, err
		}
	}
	if w.dmg == "" && lo == 0 && kb.hasLo {
		if err := w.checkSibling(kb, false); err != nil {
			return Row{}, false, err
		}
	}
	return w.absent(n.pgno)
}

// checkSibling reads the leaf next to the one the search ended in (after it when next, else before it) and
// proves that its rows respect the bound they share with n: the first row of the
// next leaf must be above kb.hi, the last row of the previous leaf at most kb.lo.
// An unreadable adjacent page or cell makes the answer uncertain; an empty
// adjacent leaf proves nothing and is accepted.
func (w *walker) checkSibling(kb keyBounds, next bool) error {
	// The frames of the path say which child was taken at each level.
	i := len(w.stack) - 1
	for ; i >= 0; i-- {
		f := w.stack[i]
		if (next && f.k < len(f.n.ptrs)) || (!next && f.k > 0) {
			break
		}
	}
	if i < 0 {
		return nil // the edge of the tree: no sibling to compare with
	}
	vis, err := newMapVisitor(w.l, w.v.e.opts.Limits.MaxBTreeDepth+2)
	if err != nil {
		return err
	}
	defer vis.release(w.l)
	sw := w.v.newWalker(w.ctx, w.l, vis, TableTree)
	defer func() { w.l.free(sw.held) }()
	f := w.stack[i]
	step := f.k - 1
	if next {
		step = f.k + 1
	}
	pgno, ok, err := sw.childAt(f.n, step)
	if err != nil {
		return err
	}
	if !ok {
		w.damage("a sibling subtree cannot be reached")
		return nil
	}
	for depth := f.n.depth + 1; ; depth++ {
		cn, found, err := sw.enter(pgno, depth)
		if err != nil {
			return err
		}
		if !found {
			w.damage("an adjacent page cannot be used")
			return nil
		}
		if !cn.h.Type.interior() {
			if len(cn.ptrs) == 0 {
				return nil
			}
			at := 0
			if !next {
				at = len(cn.ptrs) - 1
			}
			c, ok, err := sw.probe(cn, at)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				w.damage("a cell of the adjacent leaf cannot be parsed")
			case next && c.Rowid <= kb.hi:
				w.damage("the adjacent leaf holds a row below the key that bounds this one")
			case !next && c.Rowid > kb.lo:
				w.damage("the adjacent leaf holds a row above the key that bounds this one")
			}
			return nil
		}
		if next {
			if len(cn.ptrs) == 0 {
				pgno = cn.h.RightChild
				continue
			}
			pgno, ok, err = sw.childAt(cn, 0)
		} else {
			pgno = cn.h.RightChild
		}
		if err != nil {
			return err
		}
		if !ok {
			w.damage("a sibling subtree cannot be reached")
			return nil
		}
	}
}

// childAt returns the child of interior page n at position i (len(n.ptrs) is
// the right-most child); ok is false when its cell cannot be parsed.
func (w *walker) childAt(n node, i int) (uint32, bool, error) {
	if i >= len(n.ptrs) {
		return n.h.RightChild, true, nil
	}
	c, ok, err := w.cell(n, n.ptrs[i])
	if err != nil || !ok {
		return 0, false, err
	}
	return c.LeftChild, true, nil
}

// found decodes the row of cell c. Its overflow pages are marked in a visited
// set of their own, sized for the chain the cell declares, never above the
// shared overflow cap and never above the pages the file can supply (a chain
// visits distinct pages, so a hostile declared length cannot inflate the
// charge). A row that cannot be decoded is not "absent": the answer is
// uncertain.
func (w *walker) found(n node, ptr CellPointer, c Cell, kb keyBounds) (Row, bool, error) {
	local, spills := LocalPayload(w.v.info.UsableSize, n.h.Type, c.PayloadLen)
	pages := int64(1)
	if spills {
		pages += (c.PayloadLen-local)/int64(w.v.info.UsableSize-4) + 1
	}
	ovf, err := newMapVisitor(w.l, int(min(pages, w.v.e.overflowCap(w.v.info.UsableSize), int64(w.v.addr))))
	if err != nil {
		return Row{}, false, err
	}
	defer ovf.release(w.l)
	row, held, ok, err := w.rowFor(n, ptr, c, ovf)
	if err != nil {
		return Row{}, false, err
	}
	if !ok {
		return Row{}, false, w.uncertain(n.pgno, fmt.Sprintf("the row of rowid %d cannot be decoded", c.Rowid))
	}
	w.l.free(held) // the row now belongs to the caller
	if !kb.holds(c.Rowid) {
		row.KeyRangeViolation = true
		w.warnKeyRange(n, c, kb)
	}
	return row, true, nil
}
