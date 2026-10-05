package sqlitefile

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// sqliteSchemaDef is the fixed definition of the schema table itself, which
// has no row of its own in the schema.
var sqliteSchemaDef = func() *TableDef {
	names := []string{"type", "name", "tbl_name", "rootpage", "sql"}
	types := []string{"text", "text", "text", "integer", "text"}
	d := &TableDef{RowidAlias: -1, StoredColumns: len(names), ParseOK: true}
	for i, n := range names {
		d.Columns = append(d.Columns, Column{Name: n, DeclType: types[i], Affinity: AffinityOf(types[i]), RecordIndex: i})
	}
	return d
}()

// Table is a table of the schema, ready to be read.
type Table struct {
	v   *View
	obj SchemaObject
}

// Index is an index of the schema, ready to be read.
type Index struct {
	v   *View
	obj SchemaObject
}

// Table finds the table called name (ASCII case-insensitive; "sqlite_master"
// is the schema table's old name). A view, trigger, virtual table or an
// unknown name is ErrNotFound.
func (v *View) Table(ctx context.Context, name string) (t *Table, err error) {
	defer guard(&err)
	if asciiEqualFold(name, "sqlite_master") || asciiEqualFold(name, "sqlite_schema") {
		return &Table{v: v, obj: SchemaObject{Type: "table", Name: "sqlite_schema", TblName: "sqlite_schema", RootPage: 1, Table: sqliteSchemaDef}}, nil
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
		if o.Virtual || o.RootPage == 0 {
			return nil, fmt.Errorf("%w: %q is a virtual table and has no b-tree", ErrNotFound, name)
		}
		return &Table{v: v, obj: o}, nil
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
	return t.v.ScanTree(ctx, t.obj.RootPage, t.kind(), visit)
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
			out[i] = columnDefault(c, enc)
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
func (i *Index) Entries(ctx context.Context, visit func(Row) bool) error {
	return i.v.ScanTree(ctx, i.obj.RootPage, IndexTree, visit)
}

// columnDefault is the value a short record gives column c.
func columnDefault(c *Column, enc Encoding) Value {
	switch c.Default.Kind {
	case DefaultNone:
		return Value{}
	case DefaultExpr:
		return Value{Omitted: true}
	}
	v, ok := applyAffinity(c.Default.Value, c.Affinity)
	if !ok {
		return Value{Omitted: true}
	}
	switch v.Kind {
	case KindText:
		b := encodeTextAs(string(v.Bytes), enc)
		v.Bytes, v.Len, v.Enc = b, int64(len(b)), enc
	case KindBlob:
		v.Bytes = bytes.Clone(v.Bytes)
	}
	return v
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

// applyAffinity converts v as the engine does when it stores a value in a
// column of affinity aff. ok is false for a conversion this reader does not
// reproduce (a real turned into TEXT, whose spelling is the engine's).
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
		switch v.Kind {
		case KindText:
			if n, ok := numericText(string(v.Bytes)); ok {
				v = n
			}
		}
		if v.Kind == KindFloat && aff != AffReal {
			if f := v.Float; f == math.Trunc(f) && math.Abs(f) < 9.2e18 {
				v = Value{Kind: KindInt, Int: int64(f)}
			}
		}
		if v.Kind == KindInt && aff == AffReal {
			v = Value{Kind: KindFloat, Float: float64(v.Int)}
		}
	}
	return v, true
}

// numericText reads text that is, in whole (surrounding blanks allowed), a
// decimal number: an integer that fits 64 bits is an integer, any other is a
// real. Hexadecimal, "inf", "nan" and underscores are not numbers.
func numericText(s string) (Value, bool) {
	s = strings.Trim(s, " \t\n\r\f\v")
	if s == "" {
		return Value{}, false
	}
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	digits, frac := 0, 0
	for i < len(s) && isDigit(s[i]) {
		i++
		digits++
	}
	isReal := false
	if i < len(s) && s[i] == '.' {
		isReal = true
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
			frac++
		}
	}
	if digits+frac == 0 {
		return Value{}, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j {
			return Value{}, false
		}
		isReal, i = true, k
	}
	if i != len(s) {
		return Value{}, false
	}
	if !isReal {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return Value{Kind: KindInt, Int: n}, true
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return Value{}, false
	}
	return Value{Kind: KindFloat, Float: f}, true
}
