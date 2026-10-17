package sqlitefile

import (
	"context"
	"fmt"
	"slices"
)

// KeyOrder says how the entries of an index-shaped tree are ordered: the key columns (the first
// len(Cols) values of every record) and whether two equal keys are damage (the primary key of a
// WITHOUT ROWID table, a unique index) or a legitimate tie (a non-unique index, a unique index
// whose key holds a NULL).
type KeyOrder struct {
	Cols   []KeyColumn
	Unique bool

	// unchecked, when set, says why this tree's order cannot be checked at all (an
	// automatic index, a definition that is not parsed): every entry is counted.
	unchecked string
}

// orderState is the key-order check of one scan: the previous entry's key, the ledger
// charge for it, and what could not be compared.
type orderState struct {
	order     KeyOrder
	prev      []Value // the key columns of the previous entry, owned and charged
	prevHeld  int64
	havePrev  bool
	unchecked int64
	counted   bool   // the entry being checked could not be compared
	why       string // the first reason an entry could not be compared
}

// ScanTreeOrdered is ScanTreeLoss for an index-shaped tree that also checks the entries against order.
// The in-order walk of a valid index tree yields keys that never decrease, so each entry is compared
// with the one before it: an entry below its predecessor (or equal to it in a unique key without a
// NULL) is delivered with KeyRangeViolation set and a btree-order warning; an entry the comparator
// cannot decide (a custom collation, an unread value, a record shorter than the key) is counted, and
// one key-order-unchecked warning per scan states the count and the first reason. Such an entry is
// neither damage nor known to be fine.
func (v *View) ScanTreeOrdered(ctx context.Context, root uint32, order KeyOrder, visit func(Row) bool) (ScanLoss, error) {
	return v.scanTree(ctx, root, IndexTree, &order, visit)
}

// checkOrder compares row with the previous entry of the scan and with the bounds lo and hi its
// ancestors' separators give its subtree (either may be nil), and keeps row as the new previous
// entry. It sets row.KeyRangeViolation and warns for an entry out of order: below the previous
// entry, or outside (lo, hi) (a tie with a bound is damage only where the key is unique). The
// predecessor check finds the entry that descends; the range check finds the entries that sit in
// the wrong subtree whatever their neighbours are, a misplaced run included. An entry for which
// one of these comparisons cannot be decided is counted once.
func (w *walker) checkOrder(row *Row, lo, hi *boundKey) error {
	o := w.ord
	if o == nil {
		return nil
	}
	w.v.e.at("scan.order")
	cols := o.order.Cols
	switch {
	case o.order.unchecked != "":
		o.uncheck(o.order.unchecked)
		return nil
	case len(cols) == 0:
		o.uncheck("the key has no columns")
		return nil
	}
	strict := o.order.Unique && !keyHasNull(row.Values, len(cols))
	undecided := func(a, b []Value, n int) {
		if o.why == "" {
			o.why = undecidedReason(a, b, cols, n)
		}
		o.counted = true
	}
	o.counted = false
	if o.havePrev {
		w.v.st.orderCompares.Add(1)
		c, n, st := OrderKeys(o.prev, row.Values, cols)
		switch {
		case st != OrderOK:
			undecided(o.prev, row.Values, n)
		case c > 0:
			w.orderViolation(row, "is below the previous entry")
		case c == 0 && strict:
			w.orderViolation(row, "repeats the key of the previous entry in a unique key")
		}
	}
	if lo != nil {
		w.v.st.orderCompares.Add(1)
		c, n, st := OrderKeys(lo.vals, row.Values, cols)
		switch {
		case st != OrderOK:
			undecided(lo.vals, row.Values, n)
		case (c > 0 || (c == 0 && strict)) && !row.KeyRangeViolation:
			w.orderViolation(row, "is not above the entry that bounds its subtree from below")
		}
	}
	if hi != nil {
		w.v.st.orderCompares.Add(1)
		c, n, st := OrderKeys(row.Values, hi.vals, cols)
		switch {
		case st != OrderOK:
			undecided(row.Values, hi.vals, n)
		case (c > 0 || (c == 0 && strict)) && !row.KeyRangeViolation:
			w.orderViolation(row, "is not below the entry that bounds its subtree from above")
		}
	}
	if o.counted {
		o.unchecked++
	}
	return w.keepPrev(row)
}

func (o *orderState) uncheck(why string) {
	if o.why == "" {
		o.why = why
	}
	o.unchecked++
}

func (w *walker) orderViolation(row *Row, what string) {
	row.KeyRangeViolation = true
	w.v.warns.add(Warning{
		Code: WarnBTreeOrder, File: row.Loc.File, Page: row.Loc.Page, Offset: row.Loc.Offset,
		Msg: fmt.Sprintf("the entry at cell %d of page %d %s (cell %d of page %d)", row.Loc.Cell, row.Loc.Page, what, w.prevCell, w.prevPage),
	})
}

// keyHasNull reports whether one of the first n values is NULL (a NaN reads as NULL).
func keyHasNull(vals []Value, n int) bool {
	for _, v := range vals[:min(n, len(vals))] {
		if isNullLike(v) {
			return true
		}
	}
	return false
}

// keepPrev replaces the previous entry by a charged clone of the key columns of row.
func (w *walker) keepPrev(row *Row) error {
	o := w.ord
	k := min(len(row.Values), len(o.order.Cols))
	cost := int64(k) * valueCost
	for _, v := range row.Values[:k] {
		cost += int64(len(v.Bytes))
	}
	w.l.free(o.prevHeld)
	o.prevHeld = 0
	if err := w.l.alloc(cost); err != nil {
		o.prev, o.havePrev = nil, false
		return err
	}
	o.prevHeld = cost
	o.prev = slices.Clone(row.Values[:k])
	for i := range o.prev {
		if o.prev[i].Bytes != nil {
			o.prev[i].Bytes = slices.Clone(o.prev[i].Bytes)
		}
	}
	o.havePrev = true
	w.prevPage, w.prevCell = row.Loc.Page, row.Loc.Cell
	return nil
}

// finishOrder gives back the previous-entry charge and states what could not be compared.
func (w *walker) finishOrder(root uint32) {
	o := w.ord
	if o == nil {
		return
	}
	for i := range w.stack {
		w.freeBound(w.stack[i].prevB)
		w.freeBound(w.stack[i].curB)
		w.stack[i].prevB, w.stack[i].curB = nil, nil
	}
	w.l.free(o.prevHeld)
	o.prevHeld, o.prev = 0, nil
	if o.unchecked > 0 {
		w.v.warns.add(Warning{
			Code: WarnKeyOrderUnchecked, File: sourceFile(w.v.src, root), Page: root,
			Msg: fmt.Sprintf("%d entries could not be compared with the entry before them or with the bounds their ancestors give them, so their order is not checked (first reason: %s)", o.unchecked, o.why),
		})
	}
}

// undecidedReason says why OrderKeys could not decide at column n. It is built once per scan.
func undecidedReason(a, b []Value, cols []KeyColumn, n int) string {
	if min(len(a), len(b)) < len(cols) {
		return fmt.Sprintf("a record is shorter than the %d-column key", len(cols))
	}
	if n >= len(cols) {
		return "the comparison is not decided"
	}
	for _, v := range [2]Value{a[n], b[n]} {
		if v.Omitted || v.Unread || v.Clipped {
			return fmt.Sprintf("key column %d holds a value that is unread, omitted or clipped", n+1)
		}
	}
	if _, err := CanonicalCollation(cols[n].Collation); err != nil {
		return fmt.Sprintf("key column %d uses the collation %q, which this reader does not reproduce", n+1, cols[n].Collation)
	}
	if cols[n].Expr {
		return fmt.Sprintf("key column %d is an expression whose collation is not known and it holds text", n+1)
	}
	return fmt.Sprintf("key column %d holds text that is invalid in its encoding or in two encodings", n+1)
}

// withoutRowidOrder is the order of the entries of a WITHOUT ROWID table: its key columns in key
// order, under the key's collation and declared sort order. A table whose definition is not parsed
// has no known key.
func withoutRowidOrder(d *TableDef) KeyOrder {
	if d == nil || !d.ParseOK {
		return KeyOrder{unchecked: "the table definition is not parsed, so its key is unknown"}
	}
	o := KeyOrder{Unique: true}
	for _, c := range pkColumns(d) {
		o.Cols = append(o.Cols, KeyColumn{Collation: ColumnCollation(*c), Desc: c.PKDesc})
	}
	return o
}

// pkColumns are the primary key columns of d in key order.
func pkColumns(d *TableDef) []*Column {
	var keys []*Column
	for i := range d.Columns {
		if d.Columns[i].PKOrdinal > 0 {
			keys = append(keys, &d.Columns[i])
		}
	}
	slices.SortFunc(keys, func(a, b *Column) int { return a.PKOrdinal - b.PKOrdinal })
	return keys
}

// indexOrder is the order of the entries of index ix: its key columns under their collations and
// sort orders, the table's declared collation where the index names none. An automatic index
// (its columns are not stored) and an index whose table or column cannot be found are not checked.
func (v *View) indexOrder(ctx context.Context, ix *SchemaObject) (KeyOrder, error) {
	d := ix.Index
	if d == nil || !d.ParseOK || d.Auto {
		return KeyOrder{unchecked: "the index columns are not stored (an automatic index or an unparsed definition)"}, nil
	}
	s, err := v.Schema(ctx)
	if err != nil {
		return KeyOrder{}, err
	}
	var td *TableDef
	for i := range s.Objects {
		if o := &s.Objects[i]; o.Type == "table" && asciiEqualFold(o.Name, d.Table) {
			td = o.Table
			break
		}
	}
	if td == nil || !td.ParseOK {
		return KeyOrder{unchecked: "the definition of the indexed table is not parsed"}, nil
	}
	out := KeyOrder{Unique: d.Unique}
	for _, ic := range d.Columns {
		kc := KeyColumn{Desc: ic.Desc}
		switch {
		case ic.Expr:
			kc.Expr = true
		default:
			ci := slices.IndexFunc(td.Columns, func(c Column) bool { return asciiEqualFold(c.Name, ic.Name) })
			switch {
			case ci >= 0:
				kc.Collation = ic.Collation
				if kc.Collation == "" {
					kc.Collation = td.Columns[ci].Collation
				}
			case asciiEqualFold(ic.Name, "rowid") || asciiEqualFold(ic.Name, "_rowid_") || asciiEqualFold(ic.Name, "oid"):
				// the rowid: an integer
			default:
				return KeyOrder{unchecked: "an index column names no column of its table"}, nil
			}
		}
		out.Cols = append(out.Cols, kc)
	}
	return out, nil
}

// boundKey is the key of an interior entry that bounds a subtree: owned and charged.
type boundKey struct {
	vals []Value
	cost int64
}

// cloneBound keeps a charged clone of the key columns of vals.
func (w *walker) cloneBound(vals []Value) (*boundKey, error) {
	k := min(len(vals), len(w.ord.order.Cols))
	cost := int64(k) * valueCost
	for _, v := range vals[:k] {
		cost += int64(len(v.Bytes))
	}
	if err := w.l.alloc(cost); err != nil {
		return nil, err
	}
	b := &boundKey{vals: slices.Clone(vals[:k]), cost: cost}
	for i := range b.vals {
		if b.vals[i].Bytes != nil {
			b.vals[i].Bytes = slices.Clone(b.vals[i].Bytes)
		}
	}
	return b, nil
}

func (w *walker) freeBound(b *boundKey) {
	if b != nil {
		w.l.free(b.cost)
	}
}

// boundOf decodes the entry of the interior cell at ptr of f and keeps its key as the upper bound
// of the subtree to its left (f.curB). The cell is decoded again when it is delivered, with an
// overflow visitor of its own so the pages are not met twice; what this read loses is counted
// by the delivery, not here.
func (w *walker) boundOf(f *frame, ptr CellPointer) error {
	o := w.ord
	if f.curB != nil || o.order.unchecked != "" || len(o.order.Cols) == 0 {
		return nil
	}
	saved := w.loss
	row, held, ok, err := w.entryOf(f.n, ptr, f.cur)
	w.loss = saved
	if err != nil {
		return err
	}
	if !ok {
		return nil // the subtree keeps the bound its ancestors give it
	}
	defer w.l.free(held)
	f.curB, err = w.cloneBound(row.Values)
	return err
}

// shiftBound makes the entry just delivered the lower bound of the subtree to its right.
func (w *walker) shiftBound(f *frame) {
	w.freeBound(f.prevB)
	f.prevB, f.curB = f.curB, nil
}
