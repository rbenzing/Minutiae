package sqlitefile

import (
	"context"
	"fmt"
	"slices"
	"unicode/utf8"
)

// GetByKey finds the row of a WITHOUT ROWID table whose primary key equals key (one value per key
// column, in key order), equal under the key's own collations: the b-tree's notion of the same row.
// Each value takes the affinity of its key column first (ConvertForComparison with own AffBlob:
// the value is a constant), and text is put in the database's encoding. found is false only when
// the search path was proven in order: "absent from the path the key's search takes", not "absent
// from the file". An entry misplaced into another subtree is not on that path; the ordered scan
// (Table.Rows) flags every entry outside the range its ancestors give it, so a row the scan
// delivers unflagged is found by its key. Any damage on the path is a *CorruptError ("the row may
// exist"), a comparison the reader cannot make is ErrKeyUndecided, and a key of the wrong length,
// a NULL or NaN component, a table with rowids or a table whose definition is not parsed
// (ParseOK false) is ErrBadKey. The row is as Rows delivers it (key columns first) and is the
// caller's.
func (t *Table) GetByKey(ctx context.Context, key []Value) (Row, bool, error) {
	d := t.obj.Table
	if d == nil || !d.ParseOK {
		return Row{}, false, fmt.Errorf("%w: the definition of %q is not parsed, so its key is unknown", ErrBadKey, t.obj.Name)
	}
	if !d.WithoutRowid {
		return Row{}, false, fmt.Errorf("%w: %q has rowids; use Get", ErrBadKey, t.obj.Name)
	}
	pk := pkColumns(d)
	if len(pk) == 0 || len(key) != len(pk) {
		return Row{}, false, fmt.Errorf("%w: %q has a %d-column key and %d values were given", ErrBadKey, t.obj.Name, len(pk), len(key))
	}
	enc := normEnc(t.v.info.Encoding)
	probe := make([]Value, len(key))
	for i, v := range key {
		switch {
		case isNullLike(v):
			return Row{}, false, fmt.Errorf("%w: key column %d is NULL or NaN, which no row of a primary key holds", ErrBadKey, i+1)
		case v.Omitted || v.Unread || v.Clipped:
			return Row{}, false, fmt.Errorf("%w: key value %d is omitted, unread or clipped", ErrBadKey, i+1)
		}
		cv, st := ConvertForComparison(v, AffBlob, pk[i].Affinity)
		if st != ConvOK {
			return Row{}, false, fmt.Errorf("%w: key value %d cannot be converted exactly for the affinity of its key column", ErrKeyUndecided, i+1)
		}
		if cv.Kind == KindText {
			if cv.Enc == 0 {
				cv.Enc = EncUTF8
			}
			if cv.Enc == EncUTF8 && enc != EncUTF8 {
				if !utf8.Valid(cv.Bytes) {
					return Row{}, false, fmt.Errorf("%w: key value %d is text that is not valid UTF-8, so it cannot be put in the database encoding", ErrKeyUndecided, i+1)
				}
				cv.Bytes = encodeTextAs(string(cv.Bytes), enc)
				cv.Len, cv.Enc = int64(len(cv.Bytes)), enc
			}
		}
		probe[i] = cv
	}
	return t.v.LookupKey(ctx, t.obj.RootPage, withoutRowidOrder(d), probe)
}

// keyHold is a key (the first columns of an entry) the lookup keeps: owned and charged.
type keyHold struct {
	vals []Value
	cost int64
	ok   bool
}

// keyRange is the open range (lo, hi) of keys a subtree may hold, from the entries of its
// ancestors, which an index tree keeps in its interior pages.
type keyRange struct{ lo, hi keyHold }

// kframe is an interior page on the path: k is the position of the child taken (len(n.ptrs) is
// the right-most child).
type kframe struct {
	n node
	k int
}

// klook is one key lookup.
type klook struct {
	w     *walker
	probe []Value
	cols  []KeyColumn
	stack []kframe
	undec string // why an entry could not be compared with its neighbour (the first reason)
	held  int64  // the key clones still charged
}

// LookupKey finds the entry of the index-shaped tree rooted at root whose first len(order.Cols)
// values equal key under order (collations; the sort order only decides the direction of the
// search). It is GetByKey without the conversions of the key: the values are compared as given.
// It descends by comparing the key with every entry of each page on the path; "absent" (ok false,
// nil error) is answered only when every page on the path has proven order (its entries strictly
// increasing and inside the range the ancestors' entries give it) and, when the search ends at a
// side of a leaf that has a bound, the adjacent leaf's entry next to the bound lies beyond it.
// That is absence from the search path, not from the tree: an entry that sits in another subtree
// than its key belongs to is not on the path, and the ordered scan is what flags it. Damage on the path is a *CorruptError (the row may exist), a comparison the reader cannot make
// is ErrKeyUndecided, a key of the wrong length or an order that is unknown is ErrBadKey.
// An entry that is found but lies outside its bounds or beside a neighbour it contradicts is
// returned with KeyRangeViolation set. The returned Row is the caller's.
func (v *View) LookupKey(ctx context.Context, root uint32, order KeyOrder, key []Value) (row Row, ok bool, err error) {
	defer guard(&err)
	l := v.e.newLedger()
	defer l.guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Row{}, false, err
	}
	if order.unchecked != "" || len(order.Cols) == 0 || len(key) != len(order.Cols) {
		return Row{}, false, fmt.Errorf("%w: the key order is unknown or the key has %d values for %d columns", ErrBadKey, len(key), len(order.Cols))
	}
	path, err := newMapVisitor(l, v.e.opts.Limits.MaxBTreeDepth+1)
	if err != nil {
		return Row{}, false, err
	}
	defer path.release(l)
	w := v.newWalker(ctx, l, path, IndexTree)
	k := &klook{w: w, probe: key, cols: order.Cols}
	var kr keyRange
	defer func() {
		k.drop(&kr.lo)
		k.drop(&kr.hi)
		l.free(w.held)
		l.free(k.held)
	}()
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
		if n.empty {
			w.damage("a page on the search path is empty, which the engine treats as corrupt")
		}
		if depth > engineMaxBTreeDepth {
			w.damage("the search path is deeper than the engine follows, which it treats as corrupt")
		}
		interior := n.h.Type.interior()
		res, err := k.sweep(n, &kr, interior)
		if err != nil {
			return Row{}, false, err
		}
		if res.hit {
			l.free(res.held) // the row now belongs to the caller
			return res.row, true, nil
		}
		if !interior {
			if w.dmg == "" && res.pos == 0 && kr.lo.ok {
				if err := k.checkSibling(kr, false); err != nil {
					return Row{}, false, err
				}
			}
			if w.dmg == "" && res.pos == len(n.ptrs) && kr.hi.ok {
				if err := k.checkSibling(kr, true); err != nil {
					return Row{}, false, err
				}
			}
			return k.absent(n.pgno)
		}
		k.stack = append(k.stack, kframe{n: n, k: res.pos})
		k.drop(&kr.lo)
		k.drop(&kr.hi)
		kr = res.next
		pgno = res.child
	}
}

// absent is the answer of a lookup that found nothing: "absent" when the path was proven in
// order and every comparison was decided, else the error that says why it is not.
func (k *klook) absent(pgno uint32) (Row, bool, error) {
	switch {
	case k.w.dmg != "":
		return Row{}, false, k.w.uncertain(pgno, k.w.dmg)
	case k.undec != "":
		return Row{}, false, fmt.Errorf("%w: %s", ErrKeyUndecided, k.undec)
	}
	return Row{}, false, nil
}

// hold keeps a charged clone of the first columns of vals.
func (k *klook) hold(vals []Value) (keyHold, error) {
	n := min(len(vals), len(k.cols))
	cost := int64(n) * valueCost
	for _, v := range vals[:n] {
		cost += int64(len(v.Bytes))
	}
	if err := k.w.l.alloc(cost); err != nil {
		return keyHold{}, err
	}
	k.held += cost
	c := slices.Clone(vals[:n])
	for i := range c {
		if c[i].Bytes != nil {
			c[i].Bytes = slices.Clone(c[i].Bytes)
		}
	}
	return keyHold{vals: c, cost: cost, ok: true}, nil
}

// drop gives a held key back.
func (k *klook) drop(h *keyHold) {
	if h.ok {
		k.w.l.free(h.cost)
		k.held -= h.cost
	}
	*h = keyHold{}
}

// noOverflow is the visitor of a cell that has no overflow chain.
type noOverflow struct{}

func (noOverflow) mark(uint32) bool { return true }

// entryOf decodes the record of cell c of n. Its overflow pages are marked in a visited set of
// their own, sized for the chain the cell declares and never above the shared cap or the pages
// the file can supply. ok is false (and the damage noted by the caller) for a record that cannot
// be decoded; held is what the row keeps charged.
func (w *walker) entryOf(n node, ptr CellPointer, c Cell) (row Row, held int64, ok bool, err error) {
	var ovf visitor = noOverflow{}
	if _, spills := LocalPayload(w.v.info.UsableSize, n.h.Type, c.PayloadLen); spills {
		local, _ := LocalPayload(w.v.info.UsableSize, n.h.Type, c.PayloadLen)
		pages := (c.PayloadLen-local)/int64(w.v.info.UsableSize-4) + 1
		mv, err := newMapVisitor(w.l, int(min(pages, w.v.e.overflowCap(w.v.info.UsableSize), int64(w.v.addr))))
		if err != nil {
			return Row{}, 0, false, err
		}
		defer mv.release(w.l)
		ovf = mv
	}
	return w.rowFor(n, ptr, c, ovf)
}

// sweepRes is what the pass over one page of the path found.
type sweepRes struct {
	hit   bool
	row   Row
	held  int64
	pos   int    // the position of the first entry above the key (len(ptrs): none)
	child uint32 // an interior page: the child to descend into
	next  keyRange
}

// sweep parses every cell of page n, compares each entry with the key and with its neighbour and
// the bounds kr, and records damage unless the entries are strictly increasing and inside kr. An
// equal entry is a hit and ends the pass. A comparison of the key with an entry that cannot be
// made ends the lookup. For an interior page it returns the child that may hold the key and the
// range that child may hold.
func (k *klook) sweep(n node, kr *keyRange, interior bool) (res sweepRes, err error) {
	w := k.w
	var prev, lo, hi keyHold
	defer func() {
		k.drop(&prev)
		if err != nil || res.hit {
			k.drop(&lo)
			k.drop(&hi)
		}
	}()
	res.pos = len(n.ptrs)
	chosen := -1
	var chosenChild uint32
	for i, ptr := range n.ptrs {
		c, ok, err := w.cell(n, ptr)
		if err != nil {
			return res, err
		}
		if !ok {
			w.damage("a cell on the search path cannot be parsed")
			continue
		}
		row, held, ok, err := w.entryOf(n, ptr, c)
		if err != nil {
			return res, err
		}
		if !ok {
			w.damage("the record of a cell on the search path cannot be decoded")
			continue
		}
		cp, at, st := OrderKeys(k.probe, row.Values, k.cols)
		if st != OrderOK {
			w.l.free(held)
			return res, fmt.Errorf("%w: %s", ErrKeyUndecided, undecidedReason(k.probe, row.Values, k.cols, at))
		}
		bad := k.checkNeighbours(prev, kr, row.Values)
		if bad != "" {
			w.damage(bad)
		}
		if cp == 0 {
			if bad != "" {
				row.KeyRangeViolation = true
				w.v.warns.add(Warning{
					Code: WarnBTreeOrder, File: n.loc.File, Page: n.pgno, Offset: n.loc.Offset + int64(c.Offset),
					Msg: fmt.Sprintf("the entry at cell %d of page %d is the one asked for and %s", ptr.Index, n.pgno, bad),
				})
			}
			res.hit, res.row, res.held = true, row, held
			return res, nil
		}
		w.l.free(held)
		if cp < 0 && chosen < 0 {
			chosen, chosenChild = i, c.LeftChild
			res.pos = i
			if interior {
				if hi, err = k.hold(row.Values); err != nil {
					return res, err
				}
				lo, prev = prev, keyHold{}
			}
		}
		if interior || i+1 < len(n.ptrs) {
			next, err := k.hold(row.Values)
			if err != nil {
				return res, err
			}
			k.drop(&prev)
			prev = next
		}
	}
	if !interior {
		return res, nil
	}
	// The child's range: the entry before the chosen one (or the last entry of the page when none
	// is above the key) is its lower bound, the chosen entry its upper bound; a side without such
	// an entry is the bound this page itself inherited.
	switch {
	case chosen >= 0:
		res.child = chosenChild
		res.next.lo, lo = lo, keyHold{}
		res.next.hi, hi = hi, keyHold{}
		if !res.next.lo.ok {
			res.next.lo, kr.lo = kr.lo, keyHold{}
		}
	default:
		res.child = n.h.RightChild
		res.next.lo, prev = prev, keyHold{}
		if !res.next.lo.ok {
			res.next.lo, kr.lo = kr.lo, keyHold{}
		}
		res.next.hi, kr.hi = kr.hi, keyHold{}
	}
	return res, nil
}

// checkNeighbours says how the entry key breaks the order of the path: it must be above the
// entry before it on its page and inside (lo, hi). "" when it does not, or when a comparison
// could not be made (which is remembered: the answer is then not "absent").
func (k *klook) checkNeighbours(prev keyHold, kr *keyRange, key []Value) string {
	reason := ""
	cmp := func(a, b []Value) int {
		c, at, st := OrderKeys(a, b, k.cols)
		if st != OrderOK {
			if k.undec == "" {
				k.undec = undecidedReason(a, b, k.cols, at)
			}
			return -1 // not a violation; the answer cannot be absent
		}
		return c
	}
	if prev.ok && cmp(prev.vals, key) >= 0 {
		reason = "the keys of a page on the search path are not strictly increasing"
	}
	if kr.lo.ok && cmp(kr.lo.vals, key) >= 0 {
		reason = "a key on the search path lies outside the range its ancestors give it"
	}
	if kr.hi.ok && cmp(key, kr.hi.vals) >= 0 {
		reason = "a key on the search path lies outside the range its ancestors give it"
	}
	return reason
}

// checkSibling reads the leaf next to the one the search ended in (after it when next, else before
// it) and proves that its entry next to the shared bound lies beyond it: the first entry of the
// next leaf must be above kr.hi, the last entry of the previous leaf below kr.lo. An unreadable
// adjacent page or cell makes the answer uncertain; an empty adjacent leaf proves nothing and is
// accepted.
func (k *klook) checkSibling(kr keyRange, next bool) error {
	w := k.w
	i := len(k.stack) - 1
	for ; i >= 0; i-- {
		f := k.stack[i]
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
	sw := w.v.newWalker(w.ctx, w.l, vis, IndexTree)
	defer func() { w.l.free(sw.held) }()
	f := k.stack[i]
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
		if cn.empty {
			w.damage("an adjacent page is empty, which the engine treats as corrupt")
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
			c, ok, err := sw.cell(cn, cn.ptrs[at])
			if err != nil {
				return err
			}
			if !ok {
				w.damage("a cell of the adjacent leaf cannot be parsed")
				return nil
			}
			row, held, ok, err := sw.entryOf(cn, cn.ptrs[at], c)
			if err != nil {
				return err
			}
			if !ok {
				w.damage("a cell of the adjacent leaf cannot be decoded")
				return nil
			}
			defer w.l.free(held)
			a, b, want := row.Values, kr.hi.vals, 1 // the next leaf's first entry is above hi
			if !next {
				a, b, want = row.Values, kr.lo.vals, -1 // the previous leaf's last entry is below lo
			}
			cmpv, at2, st := OrderKeys(a, b, k.cols)
			switch {
			case st != OrderOK:
				if k.undec == "" {
					k.undec = undecidedReason(a, b, k.cols, at2)
				}
			case cmpv != want:
				if next {
					w.damage("the adjacent leaf holds an entry below the entry that bounds this one")
				} else {
					w.damage("the adjacent leaf holds an entry above the entry that bounds this one")
				}
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
