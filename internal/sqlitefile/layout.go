package sqlitefile

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
)

// PageClass says what a page of the file is used for.
type PageClass uint8

// The page classes. The zero value is ClassOrphan: a page no structure refers to.
const (
	ClassOrphan PageClass = iota
	ClassBTreeInterior
	ClassBTreeLeaf
	ClassOverflow
	ClassFreelistTrunk
	ClassFreelistLeaf
	ClassPtrmap
	ClassLockByte
)

func (c PageClass) String() string {
	switch c {
	case ClassOrphan:
		return "orphan"
	case ClassBTreeInterior:
		return "b-tree interior page"
	case ClassBTreeLeaf:
		return "b-tree leaf page"
	case ClassOverflow:
		return "overflow page"
	case ClassFreelistTrunk:
		return "freelist trunk"
	case ClassFreelistLeaf:
		return "freelist leaf"
	case ClassPtrmap:
		return "pointer-map page"
	case ClassLockByte:
		return "lock-byte page"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// maxProblems bounds Layout.Problems.
const maxProblems = 1000

// Layout says what every page 1..Addressable is. Owner encoding (one
// definition): 0 = no owner, 1 = the sqlite_schema tree itself, k+2 =
// Schema().Objects[k]. A page claimed by two structures keeps the class of the
// first (schema tree, then the schema objects in order, then the freelist) and
// the second claim is a Problem; the page is not read through the second claim.
type Layout struct {
	PageCount    uint32      // as declared (upper bound)
	Addressable  uint32      // the size every array below is built from
	Class        []PageClass // index = page number; [0] unused; length Addressable+1
	Owner        []uint32    // see the owner encoding above
	Trunks       []uint32
	Leaves       []uint32
	PtrmapPages  []uint32
	LockBytePage uint32
	Orphans      []uint32 // in page order, at most MaxOrphans
	OrphansTotal int
	Problems     []string
}

// claimVisitor marks nothing: the layout keeps its own claim table, so every
// page the walkers meet is judged by it.
type claimVisitor struct{}

func (claimVisitor) mark(uint32) bool { return true }

// Layout walks the schema tree, every b-tree of the schema, each overflow chain,
// the freelist, the pointer-map pages and the lock-byte page and says what each
// page is. It allocates 5 bytes per addressable page (a class and an owner),
// charged to the budget until Release, never from the declared page count. In an
// auto-vacuum database every entry of the pointer map is compared with what the
// walk found (a disagreement is a ptrmap-mismatch warning). The result is read
// once per view and shared: callers must not modify it.
func (v *View) Layout(ctx context.Context) (lay *Layout, err error) {
	defer guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	v.lists.laymu.Lock()
	defer v.lists.laymu.Unlock()
	v.lists.mu.Lock()
	cached := v.lists.lay
	v.lists.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	l := v.e.newLedger()
	defer l.guard(&err)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lay, err = v.buildLayout(ctx, l)
	if err != nil {
		return nil, err
	}
	v.lists.mu.Lock()
	v.lists.lay, v.lists.layCharge = lay, l.n
	v.lists.mu.Unlock()
	return lay, nil
}

// layoutBuilder is the state of one Layout call.
type layoutBuilder struct {
	v    *View
	ctx  context.Context
	l    *ledger
	lay  *Layout
	addr uint32
	more bool // problems were cut at maxProblems
}

func (b *layoutBuilder) problem(format string, a ...any) {
	if len(b.lay.Problems) < maxProblems {
		b.lay.Problems = append(b.lay.Problems, fmt.Sprintf(format, a...))
	} else if !b.more {
		b.more = true
		b.lay.Problems = append(b.lay.Problems, "further problems are not listed")
	}
}

// claim gives page pg its class and owner. It is false (nothing set) for a page
// outside 1..Addressable, and, with a Problem, for a page already claimed.
func (b *layoutBuilder) claim(pg uint32, class PageClass, owner uint32) bool {
	if pg == 0 || pg > b.addr {
		return false
	}
	if prev := b.lay.Class[pg]; prev != ClassOrphan {
		b.problem("page %d is claimed twice: it is a %s (owner %d) and is also wanted as a %s (owner %d)", pg, prev, b.lay.Owner[pg], class, owner)
		return false
	}
	b.lay.Class[pg], b.lay.Owner[pg] = class, owner
	return true
}

// expect compares the pointer-map entry of pg with what the walk found.
func (b *layoutBuilder) expect(pg uint32, typ PtrmapType, parent uint32) error {
	v := b.v
	if v.info.AutoVacuum == AVNone || pg < 3 {
		return nil
	}
	gt, gp, ok, err := v.PtrmapEntry(b.ctx, pg)
	switch {
	case err != nil && !isUnavailable(err):
		return err
	case err != nil:
		v.warn(WarnPtrmapMismatch, pg, "page %d: its pointer-map entry cannot be read", pg)
	case !ok:
		v.warn(WarnPtrmapMismatch, pg, "page %d has no pointer-map entry; the layout says type %d parent %d", pg, typ, parent)
	case gt != typ || gp != parent:
		v.warn(WarnPtrmapMismatch, pg, "page %d: the pointer map says type %d parent %d, the layout says type %d parent %d", pg, gt, gp, typ, parent)
	}
	return nil
}

func (v *View) buildLayout(ctx context.Context, l *ledger) (*Layout, error) {
	addr := v.addr
	info := v.info
	if err := l.alloc(5 * (int64(addr) + 1)); err != nil {
		return nil, fmt.Errorf("layout of %d pages: %w", addr, err)
	}
	lay := &Layout{PageCount: info.PageCount, Addressable: addr, Class: make([]PageClass, int(addr)+1), Owner: make([]uint32, int(addr)+1)}
	b := &layoutBuilder{v: v, ctx: ctx, l: l, lay: lay, addr: addr}

	lock := LockBytePage(info.PageSize)
	if lock != 0 && lock <= addr {
		lay.Class[lock], lay.LockBytePage = ClassLockByte, lock
	}
	if info.AutoVacuum != AVNone {
		n := uint64(info.UsableSize)/5 + 1
		for r := uint64(2); r <= uint64(addr); r += n {
			pg := r
			if pg == uint64(lock) {
				pg++
			}
			if pg <= uint64(addr) && lay.Class[pg] == ClassOrphan {
				lay.Class[pg] = ClassPtrmap
				lay.PtrmapPages = append(lay.PtrmapPages, uint32(pg))
			}
		}
		if err := l.alloc(4 * int64(len(lay.PtrmapPages))); err != nil {
			return nil, err
		}
	}

	sc, err := v.Schema(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.walkTree(1, 1); err != nil {
		return nil, err
	}
	for k := range sc.Objects {
		o := &sc.Objects[k]
		if o.RootPage == 0 || (o.Type != "table" && o.Type != "index") {
			continue
		}
		if err := b.walkTree(o.RootPage, uint32(k)+2); err != nil {
			return nil, err
		}
	}

	fl, err := v.Freelist(ctx)
	if err != nil {
		return nil, err
	}
	if err := l.alloc(4 * (int64(len(fl.Trunks)) + int64(len(fl.Leaves)))); err != nil {
		return nil, err
	}
	for _, p := range fl.Trunks {
		if b.claim(p, ClassFreelistTrunk, 0) {
			lay.Trunks = append(lay.Trunks, p)
			if err := b.expect(p, PtrFree, 0); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range fl.Leaves {
		if b.claim(p, ClassFreelistLeaf, 0) {
			lay.Leaves = append(lay.Leaves, p)
			if err := b.expect(p, PtrFree, 0); err != nil {
				return nil, err
			}
		}
	}

	keep := min(int64(v.e.opts.Limits.MaxOrphans), int64(addr))
	if err := l.alloc(4 * keep); err != nil {
		return nil, err
	}
	for pg := uint32(1); pg <= addr; pg++ {
		if lay.Class[pg] != ClassOrphan {
			continue
		}
		lay.OrphansTotal++
		if int64(len(lay.Orphans)) < keep {
			lay.Orphans = append(lay.Orphans, pg)
		}
	}
	sort.Slice(lay.Orphans, func(i, j int) bool { return lay.Orphans[i] < lay.Orphans[j] })
	return lay, nil
}

// treeKind reads the flag byte of a root page to tell a table tree from an
// index tree (a table when it cannot be told).
func (b *layoutBuilder) treeKind(root uint32) BTreeKind {
	if root == 0 || root > b.addr {
		return TableTree
	}
	data, _, err := b.v.cache.read(root)
	base := 0
	if root == 1 {
		base = 100
	}
	if err != nil || base >= len(data) {
		return TableTree
	}
	if t := PageType(data[base]); t == PageIndexLeaf || t == PageIndexInterior {
		return IndexTree
	}
	return TableTree
}

// walkTree claims every page of the b-tree rooted at root, with its overflow
// chains, for owner.
func (b *layoutBuilder) walkTree(root, owner uint32) error {
	v := b.v
	w := v.newWalker(b.ctx, b.l, claimVisitor{}, b.treeKind(root))
	defer func() { b.l.free(w.held) }()

	// open claims and enters page pg as a tree page; ok is false when it is not
	// to be used (outside the file, claimed already, damaged).
	open := func(pg uint32, depth int, typ PtrmapType, parent uint32) (node, bool, error) {
		if pg == 0 || pg > b.addr {
			_, _, err := w.enter(pg, depth) // says why it cannot be used
			return node{}, false, err
		}
		if !b.claim(pg, ClassBTreeLeaf, owner) { // provisional: a damaged page is still referred to
			return node{}, false, nil
		}
		if err := b.expect(pg, typ, parent); err != nil {
			return node{}, false, err
		}
		n, ok, err := w.enter(pg, depth)
		if err != nil || !ok {
			return node{}, false, err
		}
		if n.h.Type.interior() {
			b.lay.Class[pg] = ClassBTreeInterior
		}
		return n, true, nil
	}
	cells := func(n node, visit func(c Cell) error) error {
		for _, ptr := range n.ptrs {
			c, ok, err := w.cell(n, ptr)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if c.OverflowHead != 0 {
				if err := b.chain(n, c, owner); err != nil {
					return err
				}
			}
			if visit != nil {
				if err := visit(c); err != nil {
					return err
				}
			}
		}
		return nil
	}

	rootType := PtrRoot
	if root == 1 {
		rootType = 0 // page 1 has no pointer-map entry
	}
	rn, ok, err := open(root, 1, rootType, 0)
	if err != nil || !ok {
		return err
	}
	if !rn.h.Type.interior() {
		defer w.release(rn)
		return cells(rn, nil)
	}
	type frame struct {
		n node
		k int
	}
	stack := []frame{{n: rn}}
	for len(stack) > 0 {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		f := &stack[len(stack)-1]
		var child uint32
		switch {
		case f.k < len(f.n.ptrs):
			ptr := f.n.ptrs[f.k]
			f.k++
			c, ok, err := w.cell(f.n, ptr)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if c.OverflowHead != 0 {
				if err := b.chain(f.n, c, owner); err != nil {
					return err
				}
			}
			child = c.LeftChild
		case f.k == len(f.n.ptrs):
			f.k++
			child = f.n.h.RightChild
		default:
			w.release(f.n)
			stack = stack[:len(stack)-1]
			continue
		}
		cn, ok, err := open(child, f.n.depth+1, PtrNonRoot, f.n.pgno)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if cn.h.Type.interior() {
			stack = append(stack, frame{n: cn})
			continue
		}
		err = cells(cn, nil)
		w.release(cn)
		if err != nil {
			return err
		}
	}
	return nil
}

// chain claims the overflow pages of cell c of page n for owner, following the
// chain only as far as the payload needs, never past the shared overflow cap.
func (b *layoutBuilder) chain(n node, c Cell, owner uint32) error {
	v := b.v
	usable := v.info.UsableSize
	local, _ := LocalPayload(usable, n.h.Type, c.PayloadLen)
	per := int64(usable - 4)
	want := (c.PayloadLen - local + per - 1) / per
	if capPages := v.e.overflowCap(usable); want > capPages {
		v.warn(WarnCellOverflowChain, n.pgno, "the overflow chain of a cell needs %d pages; %d are followed", want, capPages)
		want = capPages
	}
	cur, parent, typ := c.OverflowHead, n.pgno, PtrOverflow1
	for i := int64(0); i < want && cur != 0; i++ {
		if cur > b.addr {
			v.warn(WarnCellOverflowChain, parent, "overflow page %d is outside the file", cur)
			return nil
		}
		if !b.claim(cur, ClassOverflow, owner) {
			return nil
		}
		if err := b.expect(cur, typ, parent); err != nil {
			return err
		}
		data, _, err := v.cache.read(cur)
		if err != nil {
			if isUnavailable(err) {
				v.warn(WarnCellOverflowChain, cur, "overflow page %d cannot be read", cur)
				return nil
			}
			return err
		}
		if len(data) < 4 {
			return nil
		}
		parent, typ = cur, PtrOverflow2
		cur = binary.BigEndian.Uint32(data)
	}
	return nil
}
