package sqlitefile

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf16"
)

// sqliteSchemaDef returns a fresh copy of the fixed definition of the schema table itself, which
// has no row of its own in the schema.
func sqliteSchemaDef() *TableDef {
	names := []string{"type", "name", "tbl_name", "rootpage", "sql"}
	types := []string{"text", "text", "text", "integer", "text"}
	d := &TableDef{RowidAlias: -1, StoredColumns: len(names), ParseOK: true}
	for i, n := range names {
		d.Columns = append(d.Columns, Column{Name: n, DeclType: types[i], Affinity: AffinityOf(types[i]), RecordIndex: i})
	}
	return d
}

// Table is a table of the schema, ready to be read.
type Table struct {
	v   *View
	obj SchemaObject
	// schemaDeep: the schema table had a node deeper than the engine follows.
	schemaDeep bool
}

// Index is an index of the schema, ready to be read.
type Index struct {
	v   *View
	obj SchemaObject
}

// Table finds the table called name (ASCII case-insensitive; "sqlite_master"
// is the schema table's old name). A view, trigger or an
// unknown name is ErrNotFound; a virtual table exists, so it is ErrVirtualTable
// (never ErrNotFound: its content is defined by a module this reader does not
// interpret, which is unknown, not absent); a table whose root page cannot be a b-tree root
// is ErrCorrupt (it exists in the schema, so it is never reported absent).
func (v *View) Table(ctx context.Context, name string) (t *Table, err error) {
	defer guard(&err)
	if asciiEqualFold(name, "sqlite_master") || asciiEqualFold(name, "sqlite_schema") {
		return &Table{v: v, obj: SchemaObject{Type: "table", Name: "sqlite_schema", TblName: "sqlite_schema", RootPage: 1, Table: sqliteSchemaDef()}}, nil
	}
	s, err := v.Schema(ctx)
	if err != nil {
		return nil, err
	}
	for i := range s.Objects {
		o := s.Objects[i]
		if o.Type != "table" || !asciiEqualFold(o.Name, name) {
			continue
		}
		if o.Virtual {
			return nil, fmt.Errorf("%w: %q", ErrVirtualTable, name)
		}
		if o.RootPage < 2 || int64(o.RootPage) > v.e.opts.Limits.MaxPages {
			return nil, fmt.Errorf("%w: table %q names root page %d, which cannot be a b-tree root", ErrCorrupt, name, o.RootPage)
		}
		return &Table{v: v, obj: o, schemaDeep: s.BeyondEngineDepth}, nil
	}
	if s.Skipped > 0 {
		return nil, fmt.Errorf("%w: no table %q was found, but the schema was read incompletely (%d damage events): it may exist", ErrCorrupt, name, s.Skipped)
	}
	return nil, fmt.Errorf("%w: no table %q", ErrNotFound, name)
}

// Index finds the index called name (ASCII case-insensitive), automatic ones
// (sqlite_autoindex_*) included.
func (v *View) Index(ctx context.Context, name string) (ix *Index, err error) {
	defer guard(&err)
	s, err := v.Schema(ctx)
	if err != nil {
		return nil, err
	}
	for i := range s.Objects {
		o := s.Objects[i]
		if o.Type == "index" && asciiEqualFold(o.Name, name) && o.RootPage != 0 {
			return &Index{v: v, obj: o}, nil
		}
	}
	if s.Skipped > 0 {
		return nil, fmt.Errorf("%w: no index %q was found, but the schema was read incompletely (%d damage events): it may exist", ErrCorrupt, name, s.Skipped)
	}
	return nil, fmt.Errorf("%w: no index %q", ErrNotFound, name)
}

// Name is the table's name as the schema spells it.
func (t *Table) Name() string { return t.obj.Name }

// RootPage is the root page of the table's b-tree.
func (t *Table) RootPage() uint32 { return t.obj.RootPage }

// Def returns the parsed definition (a copy).
func (t *Table) Def() TableDef {
	d := *t.obj.Table
	d.Columns = slices.Clone(d.Columns)
	return d
}

// kind is the shape of the table's tree: a rowid table is a table tree, a
// WITHOUT ROWID table an index-shaped one. When the definition could not be
// parsed the root page's flag byte decides.
func (t *Table) kind() BTreeKind {
	if d := t.obj.Table; d != nil && d.ParseOK {
		if d.WithoutRowid {
			return IndexTree
		}
		return TableTree
	}
	pg, err := t.v.ReadPage(t.obj.RootPage)
	base := 0
	if t.obj.RootPage == 1 {
		base = 100
	}
	if err == nil && base < len(pg.Data) {
		if pt := PageType(pg.Data[base]); pt == PageIndexLeaf || pt == PageIndexInterior {
			return IndexTree
		}
	}
	return TableTree
}

// Rows visits the rows of the table's b-tree as stored (see View.ScanTree):
// Row.Values holds the record, Resolve turns it into declared columns. visit
// returns false to stop.
func (t *Table) Rows(ctx context.Context, visit func(Row) bool) error {
	_, err := t.RowsLoss(ctx, visit)
	return err
}

// Get finds the row with the given rowid. A WITHOUT ROWID table has no rowids:
// ErrWithoutRowid. Get reads the row's overflow chain as the engine does,
// whatever else points at the same pages: an overflow chain shared by two rows
// is detected only by a scan (Rows, View.ScanTree) or by verify, never by Get.
func (t *Table) Get(ctx context.Context, rowid int64) (Row, bool, error) {
	if t.kind() == IndexTree {
		return Row{}, false, fmt.Errorf("%w: %q", ErrWithoutRowid, t.obj.Name)
	}
	return t.v.LookupRowid(ctx, t.obj.RootPage, rowid)
}

// Resolve reads a stored row as the engine reads it, in declared column order
// (len == len(Def().Columns)): a virtual generated column is not stored and is
// Omitted; the rowid alias reads as the rowid; a column past the end of a
// short record takes its DEFAULT with the column's affinity applied (a
// non-literal default is Omitted); a REAL-affinity column holding an integer
// reads as a float; WITHOUT ROWID rows are mapped back from key-first order;
// values beyond the table's columns are ignored (they stay in Row.Values). When
// the definition could not be parsed the stored values are returned as they
// are. The values alias those of r.
func (t *Table) Resolve(r Row) []Value {
	d := t.obj.Table
	if d == nil || !d.ParseOK {
		return r.Values
	}
	out := make([]Value, len(d.Columns))
	enc := normEnc(t.v.info.Encoding)
	for i := range d.Columns {
		c := &d.Columns[i]
		switch {
		case c.RecordIndex < 0:
			out[i] = Value{Omitted: true}
		case i == d.RowidAlias && r.HasRowid:
			out[i] = Value{Kind: KindInt, Int: r.Rowid}
		case c.RecordIndex >= len(r.Values):
			out[i], _ = columnDefault(c, enc)
		default:
			v := r.Values[c.RecordIndex]
			if c.Affinity == AffReal && v.Kind == KindInt && !v.Omitted {
				v = Value{Kind: KindFloat, Float: float64(v.Int), Serial: v.Serial}
			}
			out[i] = v
		}
	}
	return out
}

// Def returns the parsed definition of the index (a copy).
func (i *Index) Def() IndexDef {
	d := *i.obj.Index
	d.Columns = slices.Clone(d.Columns)
	return d
}

// Entries visits the entries of the index in key order; for an index of a
// rowid table the last value of an entry is the rowid.
// The entries are checked for key order against the index's columns (see
// View.ScanTreeOrdered): an entry out of order is delivered with KeyRangeViolation
// set, and the entries that cannot be compared are counted in one warning.
func (i *Index) Entries(ctx context.Context, visit func(Row) bool) error {
	order, err := i.v.indexOrder(ctx, &i.obj)
	if err != nil {
		return err
	}
	_, err = i.v.ScanTreeOrdered(ctx, i.obj.RootPage, order, visit)
	return err
}

// columnDefault is the value a short record gives column c. A default that is
// not a value (an expression, or a literal whose conversion for the column's
// affinity this reader does not reproduce exactly) is Omitted and carries the
// reason in DefaultUndecided: never a rounded number and never the unconverted
// text.
func columnDefault(c *Column, enc Encoding) (Value, DefaultReason) {
	v, r := defaultValue(c)
	if r != DefaultReasonNone {
		return Value{Omitted: true, DefaultUndecided: r}, r
	}
	switch v.Kind {
	case KindText:
		b := encodeTextAs(string(v.Bytes), enc)
		v.Bytes, v.Len, v.Enc = b, int64(len(b)), enc
	case KindBlob:
		v.Bytes = bytes.Clone(v.Bytes)
	}
	return v, DefaultReasonNone
}

// encodeTextAs encodes UTF-8 text in the database encoding.
func encodeTextAs(s string, enc Encoding) []byte {
	if enc != EncUTF16LE && enc != EncUTF16BE {
		return []byte(s)
	}
	units := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2*len(units))
	for _, u := range units {
		if enc == EncUTF16BE {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return b
}

// defaultReason says why the default of c is not a value, DefaultReasonNone when
// it is one (or there is none).
func defaultReason(c *Column) DefaultReason {
	_, r := defaultValue(c)
	return r
}

// defaultValue converts the default of c once: the converted value (UTF-8, not
// yet encoded for the database) with DefaultReasonNone, or the reason it is not
// a value.
func defaultValue(c *Column) (Value, DefaultReason) {
	switch c.Default.Kind {
	case DefaultNone:
		return Value{}, DefaultReasonNone
	case DefaultExpr:
		return Value{}, DefaultReasonExpression
	case DefaultInexactLiteral:
		return Value{}, DefaultReasonInexactLiteral
	}
	v, ok := applyAffinity(c.Default.Value, c.Affinity)
	if !ok {
		return Value{}, DefaultReasonInexactLiteral
	}
	return v, DefaultReasonNone
}

// applyAffinity converts v as the engine does when it stores a value in a
// column of affinity aff. ok is false for a conversion this reader does not
// reproduce exactly: a real turned into TEXT (its spelling is the engine's) and
// number-like text that the classifier of ConvertForComparison calls undecided
// (too many digits, too large an exponent, too long).
func applyAffinity(v Value, aff Affinity) (Value, bool) {
	switch aff {
	case AffText:
		switch v.Kind {
		case KindInt:
			return textValue(strconv.FormatInt(v.Int, 10)), true
		case KindFloat:
			return Value{}, false
		}
		return v, true
	case AffNumeric, AffInteger, AffReal:
		if v.Kind == KindText {
			n, class := numericOfText(v.Bytes)
			switch class {
			case textConverted:
				v = n
			case textUndecided:
				return Value{}, false
			}
		}
		if v.Kind == KindFloat && aff != AffReal && integralInt64Real(v.Float) {
			v = Value{Kind: KindInt, Int: int64(v.Float)}
		}
		if v.Kind == KindInt && aff == AffReal {
			v = Value{Kind: KindFloat, Float: float64(v.Int)}
		}
	}
	return v, true
}

// RowsLoss is Rows that also returns what this one scan did not deliver (see
// ScanLoss). A WITHOUT ROWID table is checked for key order against its primary key
// (see View.ScanTreeOrdered): an entry out of order is delivered with KeyRangeViolation set.
func (t *Table) RowsLoss(ctx context.Context, visit func(Row) bool) (ScanLoss, error) {
	if kind := t.kind(); kind == IndexTree {
		return t.v.ScanTreeOrdered(ctx, t.obj.RootPage, withoutRowidOrder(t.obj.Table), visit)
	}
	return t.v.ScanTreeLoss(ctx, t.obj.RootPage, TableTree, visit)
}

// SchemaBeyondEngineDepth says the table lies in a database whose schema tree is deeper
// than the engine follows (anywhere in it, not only at the table row): the engine
// refuses the whole file.
func (t *Table) SchemaBeyondEngineDepth() bool { return t.obj.BeyondEngineDepth || t.schemaDeep }
