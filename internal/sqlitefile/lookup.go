package sqlitefile

import (
	"context"
)

// LookupRowid finds the row of the table tree rooted at root whose rowid is
// rowid. It binary-searches the cells of each page on the path (so it reads at
// most one page per level plus the overflow pages of the row it returns) and
// serves interior pages from the cache. ok is false when no such row is found,
// which includes a row lost to damage on the path (a warning says so). The
// search trusts the order of the keys, as the engine does: on a page whose
// keys are out of order it can miss a row that a full scan delivers. The
// returned Row is the caller's.
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
	pgno := root
	for depth := 1; ; depth++ {
		n, found, err := w.enter(pgno, depth)
		if err != nil || !found {
			return Row{}, false, err
		}
		if !n.h.Type.interior() {
			return w.lookupLeaf(n, rowid)
		}
		w.stack = append(w.stack, frame{n: n})
		next, err := w.descend(n, rowid)
		if err != nil {
			return Row{}, false, err
		}
		pgno = next
	}
}

// descend returns the child of interior page n that holds rowid: the left
// child of the first cell whose key is not below rowid, else the right-most
// child.
func (w *walker) descend(n node, rowid int64) (uint32, error) {
	lo, hi := 0, len(n.ptrs)
	child := n.h.RightChild
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c, ok, err := w.cell(n, n.ptrs[mid])
		if err != nil {
			return 0, err
		}
		if !ok { // an unreadable cell: probe on the left of it
			hi = mid
			continue
		}
		if c.Rowid >= rowid {
			child = c.LeftChild
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return child, nil
}

// lookupLeaf binary-searches the cells of leaf n.
func (w *walker) lookupLeaf(n node, rowid int64) (Row, bool, error) {
	lo, hi := 0, len(n.ptrs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c, ok, err := w.cell(n, n.ptrs[mid])
		if err != nil {
			return Row{}, false, err
		}
		switch {
		case !ok:
			hi = mid
		case c.Rowid == rowid:
			return w.found(n, n.ptrs[mid], c)
		case c.Rowid > rowid:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return Row{}, false, nil
}

// found decodes the row of cell c. Its overflow pages are marked in a visited
// set of their own, sized for the chain the cell declares, never above the
// shared overflow cap and never above the pages the file can supply (a chain
// visits distinct pages, so a hostile declared length cannot inflate the charge).
func (w *walker) found(n node, ptr CellPointer, c Cell) (Row, bool, error) {
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
	if err != nil || !ok {
		return Row{}, false, err
	}
	w.l.free(held) // the row now belongs to the caller
	return row, true, nil
}
