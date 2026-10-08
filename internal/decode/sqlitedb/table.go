package sqlitedb

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// ColumnInfo describes one declared column of a resolved table.
type ColumnInfo struct {
	Name, DeclType string
	Affinity       sqlitefile.Affinity
	NotNull        bool
	RowidAlias     bool
	Default        sqlitefile.DefaultKind
	Virtual        bool   // generated, not stored: always StateOmitted
	Collation      string // as written in the schema: the table-level key collation if any, else the column's; "" = none declared (BINARY)
}

// Table is a resolved table: its columns found by name. It is NOT safe for
// concurrent use.
type Table struct {
	db   *DB
	lt   *sqlitefile.Table
	def  sqlitefile.TableDef
	name string
}

// asciiFold lower-cases ASCII letters only: the engine compares names that
// way, and non-ASCII bytes compare exactly.
func asciiFold(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// Table resolves the table called name (ASCII case-insensitive). Every need
// column must exist, else the error is an *UnsupportedSchemaError naming all
// the missing ones in need order; want columns are optional.
func (db *DB) Table(ctx context.Context, name string, need, want []string) (t *Table, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = nil, fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lt, err := db.view.Table(ctx, name)
	if err != nil {
		if errors.Is(err, sqlitefile.ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrNoSuchTable, err)
		}
		return nil, wrapErr(err)
	}
	def := lt.Def()
	if !def.ParseOK {
		return nil, &UnsupportedSchemaError{Table: lt.Name(), Reason: "definition not parsed: " + def.ParseNote}
	}
	t = &Table{db: db, lt: lt, def: def, name: lt.Name()}
	var missing []string
	seen := map[string]bool{}
	for _, n := range need {
		k := asciiFold(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		if t.Col(n) < 0 {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, &UnsupportedSchemaError{Table: t.name, Missing: missing}
	}
	_ = want // optional columns are looked up with Col; nothing to check
	return t, nil
}

// Name is the table's name as the schema spells it.
func (t *Table) Name() string { return t.name }

// Col returns the index of the column called name (ASCII case-insensitive), or
// -1 when this variant of the table lacks it.
func (t *Table) Col(name string) int {
	k := asciiFold(name)
	for i := range t.def.Columns {
		if asciiFold(t.def.Columns[i].Name) == k {
			return i
		}
	}
	return -1
}

// Cols returns a copy of the column descriptions in declared order.
func (t *Table) Cols() []ColumnInfo {
	out := make([]ColumnInfo, len(t.def.Columns))
	for i, c := range t.def.Columns {
		coll := c.KeyCollation
		if coll == "" {
			coll = c.Collation
		}
		out[i] = ColumnInfo{
			Name: c.Name, DeclType: c.DeclType, Affinity: c.Affinity, NotNull: c.NotNull,
			RowidAlias: i == t.def.RowidAlias, Default: c.Default.Kind,
			Virtual: c.Generated == sqlitefile.GenVirtual, Collation: coll,
		}
	}
	return slices.Clip(out)
}

// WithoutRowid reports whether the table is WITHOUT ROWID.
func (t *Table) WithoutRowid() bool { return t.def.WithoutRowid }

// RootPage is the root page of the table's b-tree.
func (t *Table) RootPage() uint32 { return t.lt.RootPage() }
