package sqlitefile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// cellPoll is how many cells are parsed between two looks at the context.
const cellPoll = 64

// node is a b-tree page entered by a walk.
type node struct {
	pgno  uint32
	data  []byte // shared, read-only
	loc   PageLoc
	h     PageHeader
	ptrs  []CellPointer // the pointers that can be followed, in array order
	depth int           // 1 for the root
	bad   int           // pointers that cannot be followed
	empty bool          // no cells below the root, or an interior root with none: the engine calls it corrupt

	charged int64 // budget held for ptrs until walker.release
}

// engineMaxBTreeDepth is the depth of a b-tree the engine can follow (its cursor
// stack holds 20 pages; a deeper tree is corrupt for it, measured by
// TestTreeDeeperThanTheEngineAllowsIsWarned). The library reads deeper trees up
// to Limits.MaxBTreeDepth and warns.
const engineMaxBTreeDepth = 20

// ptrCost is what one followable cell pointer costs in the budget (the
// CellPointer struct).
const ptrCost = 24

// keyBounds is the open-closed range (lo, hi] of rowids a table subtree may
// hold, from the separator keys of its ancestors; loPg and hiPg are the
// interior pages the bounds came from. The zero value bounds nothing.
type keyBounds struct {
	lo, hi       int64
	hasLo, hasHi bool
	loPg, hiPg   uint32
}

// holds reports whether rowid lies inside the bounds.
func (b keyBounds) holds(rowid int64) bool {
	return (!b.hasLo || rowid > b.lo) && (!b.hasHi || rowid <= b.hi)
}

// owner is the interior page whose key the rowid breaks.
func (b keyBounds) owner(rowid int64) uint32 {
	if b.hasLo && rowid <= b.lo {
		return b.loPg
	}
	return b.hiPg
}

// child narrows b to the child between the keys prev (when havePrev) and key
// (when haveKey) of interior page pg.
func (b keyBounds) child(pg uint32, prev int64, havePrev bool, key int64, haveKey bool) keyBounds {
	if havePrev && (!b.hasLo || prev > b.lo) {
		b.lo, b.hasLo, b.loPg = prev, true, pg
	}
	if haveKey && (!b.hasHi || key < b.hi) {
		b.hi, b.hasHi, b.hiPg = key, true, pg
	}
	return b
}

// frame is an interior page on the walk's stack. k counts the cells handled:
// cell k is next; k == len(ptrs) means the right-most child is next.
type frame struct {
	n    node
	k    int
	cur  Cell // the parsed cell k
	have bool
	emit bool // index tree: child k is done, entry k comes next
	// table tree: the range this page's subtree may hold, and the previous key.
	kb       keyBounds
	prevKey  int64
	havePrev bool
}

// walker is one traversal of a b-tree. It is used by one goroutine and one
// call; everything it charges goes through its ledger.
type walker struct {
	v     *View
	ctx   context.Context
	l     *ledger
	vis   visitor // marks every b-tree page
	kind  BTreeKind
	stack []frame

	cells     int
	leafDepth int
	lastRowid int64
	haveRowid bool

	held         int64  // pointer-list charges not yet released
	checkOverlap bool   // a scan checks that the cells of a page do not overlap
	dmg          string // a lookup: why its answer may not be "absent" (first reason)
}

func (v *View) newWalker(ctx context.Context, l *ledger, vis visitor, kind BTreeKind) *walker {
	return &walker{v: v, ctx: ctx, l: l, vis: vis, kind: kind}
}

// onStack reports whether pgno is an interior page the walk is inside of.
func (w *walker) onStack(pgno uint32) bool {
	for i := range w.stack {
		if w.stack[i].n.pgno == pgno {
			return true
		}
	}
	return false
}

// skipPage records that a page was not used as part of the tree.
func (w *walker) skipPage(code string, pgno uint32, format string, a ...any) {
	w.v.warn(code, pgno, format, a...)
	w.v.st.pagesSkipped.Add(1)
}

func corruptReason(err error) string {
	var ce *CorruptError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return err.Error()
}

// enter validates and reads page pgno as a node at the given depth. A page
// that cannot be used is skipped with a warning (ok is false, err nil); err
// is only for an I/O error, a refused budget charge or a cancelled context.
func (w *walker) enter(pgno uint32, depth int) (n node, ok bool, err error) {
	v := w.v
	if err := w.ctx.Err(); err != nil {
		return node{}, false, err
	}
	info := v.info
	switch {
	case pgno == 0 || pgno > info.PageCount:
		w.skipPage(WarnPageRange, pgno, "page number %d is outside 1..%d", pgno, info.PageCount)
		return node{}, false, nil
	case pgno == LockBytePage(info.PageSize):
		w.skipPage(WarnPageRange, pgno, "page %d is the lock-byte page, never part of a tree", pgno)
		return node{}, false, nil
	case info.AutoVacuum != AVNone && isPtrmapPage(info.PageSize, info.Reserved, pgno):
		w.skipPage(WarnPageRange, pgno, "page %d is a pointer-map page, never part of a tree", pgno)
		return node{}, false, nil
	case depth > 1 && pgno == 1:
		w.skipPage(WarnPageRange, pgno, "page 1 is the root of the schema table, never a child")
		return node{}, false, nil
	case pgno > v.addr:
		w.skipPage(WarnPageUnavailable, pgno, "page %d is not in the file (%d pages can be read)", pgno, v.addr)
		return node{}, false, nil
	case depth > v.e.opts.Limits.MaxBTreeDepth:
		w.skipPage(WarnBTreeDepth, pgno, "the tree is deeper than %d levels; this subtree is skipped", v.e.opts.Limits.MaxBTreeDepth)
		return node{}, false, nil
	}
	if depth > engineMaxBTreeDepth {
		v.warn(WarnBTreeDepth, pgno, "the tree is %d levels deep here; the engine allows %d and calls a deeper tree corrupt (it is read regardless, up to %d levels)", depth, engineMaxBTreeDepth, v.e.opts.Limits.MaxBTreeDepth)
	}
	if !w.vis.mark(pgno) {
		if w.onStack(pgno) {
			w.skipPage(WarnBTreeCycle, pgno, "page %d is its own ancestor", pgno)
		} else {
			w.skipPage(WarnBTreeShape, pgno, "page %d was already met: it is shared by two parents or is also an overflow page", pgno)
		}
		return node{}, false, nil
	}
	v.e.at("scan.page")
	data, loc, err := v.cache.read(pgno)
	if err != nil {
		if isUnavailable(err) {
			w.skipPage(WarnPageUnavailable, pgno, "page %d cannot be read: %v", pgno, err)
			return node{}, false, nil
		}
		return node{}, false, err
	}
	h, err := ParsePageHeader(data, pgno)
	if err != nil {
		code := WarnCellPointer // a header or a pointer array that does not fit
		base := 0
		if pgno == 1 {
			base = 100
		}
		if base < len(data) && !PageType(data[base]).valid() {
			code = WarnPageTypeInvalid
		}
		w.skipPage(code, pgno, "%s", corruptReason(err))
		return node{}, false, nil
	}
	if (w.kind == TableTree) != (h.Type == PageTableLeaf || h.Type == PageTableInterior) {
		w.skipPage(WarnPageTypeInvalid, pgno, "page type %#02x does not belong to this kind of tree", uint8(h.Type))
		return node{}, false, nil
	}
	set, err := CellPointers(data, h, info.UsableSize)
	if err != nil {
		w.skipPage(WarnCellPointer, pgno, "%s", corruptReason(err))
		return node{}, false, nil
	}
	if len(set.Bad) > 0 { // one warning for the page; its good cells are read
		v.warn(WarnCellPointer, pgno, "%s; the other cells are read", corruptReason(set.Err()))
	}
	if k := set.BelowContent(); k > 0 { // the engine reads these cells; one warning for the page
		v.warn(WarnCellPointer, pgno, "%d cell pointers lie below the stored content start %d; the cells are read, as the engine reads them", k, h.ContentStart)
	}
	n = node{pgno: pgno, data: data, loc: loc, h: h, ptrs: set.Good, depth: depth, bad: len(set.Bad)}
	if h.CellCount == 0 && pgno != 1 && (depth > 1 || h.Type.interior()) {
		// The engine treats an empty page below the root, and an empty
		// interior root, as corrupt: its rows and subtree would vanish
		// silently, and a lookup through it must never be a clean absent.
		n.empty = true
		where := "that is an interior root"
		if depth > 1 {
			where = "below the root"
		}
		v.warn(WarnBTreeShape, pgno, "empty b-tree page %s: the engine treats it as corrupt; the rows it should hold are not delivered", where)
	}
	if w.checkOverlap {
		if err := w.warnOverlap(n); err != nil {
			return node{}, false, err
		}
	}
	n.charged = int64(len(n.ptrs)) * ptrCost
	if err := w.l.alloc(n.charged); err != nil {
		return node{}, false, err
	}
	w.held += n.charged
	return n, true, nil
}

// release gives back the budget held for the pointer list of n.
func (w *walker) release(n node) {
	w.l.free(n.charged)
	w.held -= n.charged
}

// regionCost is what one cell region costs while a page is checked for overlap.
const regionCost = 16

// warnOverlap parses every followable cell of n and warns once for the page when
// two cells overlap (the same cell reached by two pointers, or a cell that
// starts inside another). The cells are read regardless; this only reports.
func (w *walker) warnOverlap(n node) error {
	if len(n.ptrs) < 2 {
		return nil
	}
	cost := int64(len(n.ptrs)) * regionCost
	if err := w.l.alloc(cost); err != nil {
		return err
	}
	defer w.l.free(cost)
	type region struct{ off, end int }
	regs := make([]region, 0, len(n.ptrs))
	for _, ptr := range n.ptrs {
		c, err := ParseCell(n.data, w.v.info.UsableSize, n.h, ptr.Offset)
		if err != nil {
			continue // reported when the cell is read
		}
		regs = append(regs, region{ptr.Offset, ptr.Offset + c.Length})
	}
	slices.SortFunc(regs, func(a, b region) int { return cmp.Compare(a.off, b.off) })
	overlaps, first := 0, [2]int{}
	for i := 1; i < len(regs); i++ {
		if regs[i].off < regs[i-1].end {
			if overlaps == 0 {
				first = [2]int{regs[i-1].off, regs[i].off}
			}
			overlaps++
		}
	}
	if overlaps > 0 {
		w.v.warn(WarnCellPointer, n.pgno, "%d cells overlap another (first: the cells at offsets %d and %d); all are read", overlaps, first[0], first[1])
	}
	return nil
}

// cell parses the cell at ptr of n, warning and reporting false when it cannot
// be parsed.
func (w *walker) cell(n node, ptr CellPointer) (Cell, bool, error) {
	w.cells++
	w.v.st.cellsParsed.Add(1)
	if w.cells%cellPoll == 0 {
		if err := w.ctx.Err(); err != nil {
			return Cell{}, false, err
		}
	}
	c, err := ParseCell(n.data, w.v.info.UsableSize, n.h, ptr.Offset)
	if err != nil {
		w.v.warn(WarnCellPointer, n.pgno, "cell %d: %s", ptr.Index, corruptReason(err))
		return Cell{}, false, nil
	}
	c.Index = ptr.Index
	return c, true, nil
}

// rowFor decodes the record of cell c. A cell that cannot yield a row (an
// invalid record, a payload over the cap) is skipped with a warning: ok is
// false and err nil. held is what the row keeps charged: the caller frees it
// when it is done with the row.
func (w *walker) rowFor(n node, ptr CellPointer, c Cell, ovf visitor) (row Row, held int64, ok bool, err error) {
	v := w.v
	p := newPayload(v.cache, w.l, ovf, v.info.UsableSize, v.e.overflowCap(v.info.UsableSize), c)
	p.ctx = w.ctx
	defer p.release()
	at := cellCtx{File: n.loc.File, Page: n.pgno, Offset: n.loc.Offset + int64(c.Offset)}
	v.e.at("scan.record")
	rec, held, err := v.e.readRecord(w.l, v.warns, at, p, v.info.Encoding, nil)
	if err != nil {
		switch {
		case errors.Is(err, ErrCorrupt):
			v.warns.add(Warning{Code: WarnRecordInvalid, File: at.File, Page: at.Page, Offset: at.Offset, Msg: fmt.Sprintf("cell %d: %s", ptr.Index, corruptReason(err))})
			return Row{}, 0, false, nil
		case errors.Is(err, ErrLimit):
			v.warns.add(Warning{Code: WarnLimitReached, File: at.File, Page: at.Page, Offset: at.Offset, Msg: fmt.Sprintf("cell %d: %v", ptr.Index, err)})
			return Row{}, 0, false, nil
		}
		return Row{}, 0, false, err
	}
	if _, _, dead := p.damaged(); rec.Truncated && !dead {
		// The record declares more bytes than the cell holds and the chain is
		// intact: there is nothing to keep. (A damaged chain is different: the
		// values before it are kept and the rest flagged Omitted.)
		w.l.free(held)
		v.warns.add(Warning{Code: WarnRecordInvalid, File: at.File, Page: at.Page, Offset: at.Offset, Msg: fmt.Sprintf("cell %d: the record declares more bytes than the payload holds", ptr.Index)})
		return Row{}, 0, false, nil
	}
	row = Row{
		Rowid: c.Rowid, HasRowid: c.HasRowid, Values: rec.Values, PayloadLen: c.PayloadLen,
		Loc:            newLoc(n.loc, n.pgno, ptr.Index, c, p.provenance(), v.e.opts.Limits.MaxLocOverflow),
		LengthMismatch: rec.LengthMismatch,
	}
	return row, held, true, nil
}

// leafRows delivers the cells of leaf n. stop is true when visit asked to end.
func (w *walker) leafRows(n node, ovf visitor, kb keyBounds, visit func(Row) bool) (stop bool, err error) {
	defer w.release(n)
	if w.leafDepth == 0 {
		w.leafDepth = n.depth
	} else if w.leafDepth != n.depth {
		w.v.warn(WarnBTreeShape, n.pgno, "leaf at depth %d, its siblings are at depth %d", n.depth, w.leafDepth)
	}
	for _, ptr := range n.ptrs {
		c, ok, err := w.cell(n, ptr)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		violation := false
		if c.HasRowid {
			if w.haveRowid && c.Rowid <= w.lastRowid {
				w.v.warn(WarnBTreeOrder, n.pgno, "rowids are not in increasing order")
			}
			w.lastRowid, w.haveRowid = c.Rowid, true
			if !kb.holds(c.Rowid) {
				violation = true
				w.warnKeyRange(n, c, kb)
			}
		}
		row, held, ok, err := w.rowFor(n, ptr, c, ovf)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		row.KeyRangeViolation = violation
		more := visit(row)
		w.l.free(held)
		if !more {
			return true, nil
		}
	}
	return false, nil
}

// warnKeyRange reports a row whose rowid is outside the range its ancestors'"'"' keys
// give its subtree; the warning belongs to the interior page whose key is broken.
func (w *walker) warnKeyRange(n node, c Cell, kb keyBounds) {
	w.v.warns.add(Warning{
		Code: WarnBTreeOrder, File: n.loc.File, Page: kb.owner(c.Rowid), Offset: n.loc.Offset + int64(c.Offset),
		Msg: fmt.Sprintf("rowid %d on page %d is outside the key range its parents give it (%s)", c.Rowid, n.pgno, kb.describe()),
	})
}

// ScanTree visits the rows of the b-tree rooted at root: a table tree's leaf
// rows in tree order, an index tree's entries in order (an interior entry
// between its children). One visited set marks every b-tree and overflow page
// of the walk, so a cycle or a page shared by two structures is met once and
// skipped; damage (a bad page, pointer, cell or record) is skipped with a
// warning and never fails the scan. Row.Values is valid only during visit
// (Clone keeps a row). visit returns false to end the scan early. The error is
// for a cancelled context, an I/O error or a refused budget charge. Index
// entries are not checked for order (that needs the collations).
func (v *View) ScanTree(ctx context.Context, root uint32, kind BTreeKind, visit func(Row) bool) (err error) {
	defer guard(&err)
	l := v.e.newLedger()
	defer l.guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ps, err := newPageSet(l, v.addr)
	if err != nil {
		return err
	}
	defer ps.release(l)
	w := v.newWalker(ctx, l, ps, kind)
	w.checkOverlap = true
	defer func() { l.free(w.held) }() // pointer lists of the pages still open when the scan ends
	n, ok, err := w.enter(root, 1)
	if err != nil || !ok {
		return err
	}
	if !n.h.Type.interior() {
		_, err = w.leafRows(n, ps, keyBounds{}, visit)
		return err
	}
	w.stack = append(w.stack, frame{n: n})
	for len(w.stack) > 0 {
		f := &w.stack[len(w.stack)-1]
		if err := ctx.Err(); err != nil {
			return err
		}
		var child uint32
		var cb keyBounds // the range the child may hold (table trees)
		switch {
		case f.k < len(f.n.ptrs):
			ptr := f.n.ptrs[f.k]
			if !f.have {
				c, ok, err := w.cell(f.n, ptr)
				if err != nil {
					return err
				}
				if !ok {
					f.k++
					continue
				}
				f.cur, f.have = c, true
			}
			if f.emit { // index tree: the child before this entry is done
				cur := f.cur
				f.emit, f.have = false, false
				f.k++
				row, held, ok, err := w.rowFor(f.n, ptr, cur, ps)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				more := visit(row)
				l.free(held)
				if !more {
					return nil
				}
				continue
			}
			child = f.cur.LeftChild
			if kind == IndexTree {
				f.emit = true
			} else {
				key := f.cur.Rowid
				if f.havePrev && key <= f.prevKey {
					v.warn(WarnBTreeOrder, f.n.pgno, "interior keys are not increasing (%d after %d)", key, f.prevKey)
				}
				cb = f.kb.child(f.n.pgno, f.prevKey, f.havePrev, key, true)
				f.prevKey, f.havePrev = key, true
				f.have = false
				f.k++
			}
		case f.k == len(f.n.ptrs):
			f.k++
			child = f.n.h.RightChild
			cb = f.kb.child(f.n.pgno, f.prevKey, f.havePrev, 0, false)
		default:
			w.release(f.n)
			w.stack = w.stack[:len(w.stack)-1]
			continue
		}
		depth := f.n.depth + 1
		cn, ok, err := w.enter(child, depth)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if !cn.h.Type.interior() {
			stop, err := w.leafRows(cn, ps, cb, visit)
			if err != nil || stop {
				return err
			}
			continue
		}
		w.stack = append(w.stack, frame{n: cn, kb: cb})
	}
	return nil
}

// describe renders the bounds for a warning (numbers only).
func (b keyBounds) describe() string {
	lo, hi := "-inf", "+inf"
	if b.hasLo {
		lo = strconv.FormatInt(b.lo, 10)
	}
	if b.hasHi {
		hi = strconv.FormatInt(b.hi, 10)
	}
	return "above " + lo + ", up to " + hi
}
