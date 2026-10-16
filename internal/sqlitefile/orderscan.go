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

// checkOrder compares row with the previous entry of the scan and keeps row as the new previous
// entry. It sets row.KeyRangeViolation and warns for an entry out of order.
func (w *walker) checkOrder(row *Row) error {
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
	if o.havePrev {
		w.v.st.orderCompares.Add(1)
		c, n, st := OrderKeys(o.prev, row.Values, cols)
		switch {
		case st != OrderOK:
			if o.why == "" {
				o.why = undecidedReason(o.prev, row.Values, cols, n)
			}
			o.unchecked++
		case c > 0:
			w.orderViolation(row, "is below the previous entry")
		case c == 0 && o.order.Unique && !keyHasNull(row.Values, len(cols)):
			w.orderViolation(row, "repeats the key of the previous entry in a unique key")
		}
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
	w.l.free(o.prevHeld)
	o.prevHeld, o.prev = 0, nil
	if o.unchecked > 0 {
		w.v.warns.add(Warning{
			Code: WarnKeyOrderUnchecked, File: sourceFile(w.v.src, root), Page: root,
			Msg: fmt.Sprintf("%d entries could not be compared with the entry before them, so their order is not checked (first reason: %s)", o.unchecked, o.why),
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
	type pk struct {
		ord int
		col KeyColumn
	}
	var keys []pk
	for i := range d.Columns {
		c := &d.Columns[i]
		if c.PKOrdinal > 0 {
			keys = append(keys, pk{c.PKOrdinal, KeyColumn{Collation: ColumnCollation(*c), Desc: c.PKDesc}})
		}
	}
	slices.SortFunc(keys, func(a, b pk) int { return a.ord - b.ord })
	o := KeyOrder{Unique: true}
	for _, k := range keys {
		o.Cols = append(o.Cols, k.col)
	}
	return o
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
