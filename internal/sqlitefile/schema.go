package sqlitefile

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
)

// Affinity is a column's type affinity.
type Affinity uint8

// The five affinities; AffBlob is "none" (the BLOB affinity).
const (
	AffBlob Affinity = iota
	AffText
	AffNumeric
	AffInteger
	AffReal
)

// DefaultKind says what a column's DEFAULT is.
type DefaultKind uint8

// The kinds of default.
const (
	DefaultNone    DefaultKind = iota
	DefaultLiteral             // a constant the reader can evaluate: Value
	DefaultExpr                // CURRENT_TIMESTAMP, (expr): not evaluated
)

// Default is a column's DEFAULT clause. Value is set for DefaultLiteral (text
// in UTF-8: Table.Resolve converts it to the database encoding); Text is the
// source text of the clause, clipped.
type Default struct {
	Kind  DefaultKind
	Value Value
	Text  string
}

// GenKind says whether a column is generated.
type GenKind uint8

// The kinds of generated column.
const (
	GenNone GenKind = iota
	GenVirtual
	GenStored
)

// Column is one declared column of a table.
type Column struct {
	Name, DeclType string
	Affinity       Affinity
	NotNull        bool
	PKOrdinal      int // 0 = not part of the primary key, else 1-based position in it
	Collation      string
	Default        Default
	Generated      GenKind
	RecordIndex    int // position in the stored record; -1 for a virtual generated column
}

// TableDef is the parsed definition of a table. When ParseOK is false the
// columns are unknown (Columns is empty, RowidAlias -1) and ParseNote is a
// short token saying why.
type TableDef struct {
	Columns       []Column
	WithoutRowid  bool
	Strict        bool
	RowidAlias    int // index into Columns, -1 if none
	StoredColumns int // columns present in a full record
	ParseOK       bool
	ParseNote     string // a short token, never statement-shaped text
}

// IndexColumn is one key column of an index; Name is empty for an expression.
type IndexColumn struct {
	Name      string
	Expr      bool
	Desc      bool
	Collation string
}

// IndexDef is the parsed definition of an index. For an automatic index
// (sqlite_autoindex_*, stored with NULL sql) the columns are unknown: Auto is
// set, Unique is true and ParseOK is false.
type IndexDef struct {
	Table                 string
	Unique, Partial, Auto bool
	Columns               []IndexColumn
	ParseOK               bool
}

// SchemaObject is one row of the schema table.
type SchemaObject struct {
	Type, Name, TblName string // "table" | "index" | "view" | "trigger"
	RootPage            uint32 // 0 for views, triggers, virtual tables
	SQL                 string
	Virtual             bool
	Table               *TableDef
	Index               *IndexDef
	Loc                 Loc // where the sqlite_schema row lies
	// KeyRangeViolation: the row lies outside the key range its ancestors give it
	// (as Row.KeyRangeViolation).
	KeyRangeViolation bool
}

// Schema is the content of the schema table: the valid rows in rowid order,
// each name once (the first wins). It must not be modified.
type Schema struct {
	Objects []SchemaObject
	Cookie  uint32
	Format  uint32
}

// schemaCache holds the schema a view has read, and the budget it is charged.
type schemaCache struct {
	mu     sync.Mutex
	schema *Schema
	charge int64
}

// release drops the cached schema and gives its charge back.
func (c *schemaCache) release(b Budget) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.charge > 0 {
		b.Free(c.charge)
	}
	c.schema, c.charge = nil, 0
}

// schemaObjectCost is the fixed budget charge of one schema object (the
// struct, its definition headers and the map entry used to find duplicates).
const (
	schemaObjectCost = 512
	schemaColumnCost = 192
)

var schemaTypes = map[string]bool{"table": true, "index": true, "view": true, "trigger": true}

// nameClass keys the namespace of a schema name: tables, views and indexes
// share one, triggers have their own.
func nameClass(typ string) string {
	if typ == "trigger" {
		return "g"
	}
	return "t"
}

func asciiLower(s string) string {
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

// schemaFields validates a schema table row: type is one of the four,
// name and tbl_name are text, rootpage an integer that fits 32 bits, sql text
// or NULL. why is empty for a valid row. sqlState tells what became of the sql
// column: schemaSQLText (sql is set), schemaSQLNull, or schemaSQLUnreadable
// (an over-cap or undecodable text value).
type sqlState uint8

const (
	schemaSQLText sqlState = iota
	schemaSQLNull
	schemaSQLUnreadable
)

func schemaFields(r Row) (typ, name, tbl string, root uint32, sql string, st sqlState, why string) {
	if len(r.Values) < 5 {
		return "", "", "", 0, "", 0, fmt.Sprintf("the row has %d columns, the schema table has 5", len(r.Values))
	}
	text := func(v Value) (string, bool) {
		s, ok := v.Text()
		return s, ok
	}
	var ok bool
	if typ, ok = text(r.Values[0]); !ok || !schemaTypes[typ] {
		return "", "", "", 0, "", 0, "type is not table, index, view or trigger"
	}
	if name, ok = text(r.Values[1]); !ok || name == "" {
		return "", "", "", 0, "", 0, "name is not text"
	}
	if tbl, ok = text(r.Values[2]); !ok {
		return "", "", "", 0, "", 0, "tbl_name is not text"
	}
	rp := r.Values[3]
	if rp.Kind != KindInt || rp.Omitted || rp.Int < 0 || rp.Int > math.MaxUint32 {
		return "", "", "", 0, "", 0, "rootpage is not an integer in 0..4294967295"
	}
	root = uint32(rp.Int)
	sv := r.Values[4]
	switch sv.Kind {
	case KindNull:
		st = schemaSQLNull
	case KindText:
		if s, ok := sv.Text(); ok {
			sql, st = s, schemaSQLText
		} else {
			st = schemaSQLUnreadable
		}
	default:
		return "", "", "", 0, "", 0, "sql is neither text nor NULL"
	}
	return typ, name, tbl, root, sql, st, ""
}

// Schema reads the schema table (page 1) of the view and parses the CREATE
// statements of its tables and indexes. Invalid rows are skipped and a name
// that was met before is dropped, each with a warning; statements over
// MaxSchemaSQLBytes, or past MaxSchemaTotalBytes in all, are listed but not
// parsed; more than MaxSchemaObjects objects end the read. The result is read
// once per view and shared: callers must not modify it. A parse that fails is
// never an error: the object carries ParseOK=false and a warning is raised.
func (v *View) Schema(ctx context.Context) (s *Schema, err error) {
	defer guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	v.sch.mu.Lock()
	defer v.sch.mu.Unlock()
	if v.sch.schema != nil {
		return v.sch.schema, nil
	}
	l := v.e.newLedger()
	defer l.guard(&err)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lim := v.e.opts.Limits
	sc := &Schema{Cookie: v.info.SchemaCookie, Format: v.info.SchemaFormat}
	seen := map[string]bool{}
	var total int64
	totalWarned := false
	var ferr error
	scanErr := v.ScanTree(ctx, 1, TableTree, func(r Row) bool {
		v.e.at("schema.row")
		typ, name, tbl, root, sql, sst, why := schemaFields(r)
		at := Warning{File: r.Loc.File, Page: r.Loc.Page, Offset: r.Loc.Offset}
		if why != "" {
			at.Code, at.Msg = WarnSchemaRowInvalid, fmt.Sprintf("schema row %d skipped: %s", r.Rowid, why)
			v.warns.add(at)
			return true
		}
		key := nameClass(typ) + ":" + asciiLower(name)
		if seen[key] {
			at.Code, at.Msg = WarnSchemaDuplicate, fmt.Sprintf("a %s named %q was already read; this row is ignored", typ, name)
			v.warns.add(at)
			return true
		}
		if len(sc.Objects) >= lim.MaxSchemaObjects {
			at.Code, at.Msg = WarnLimitReached, fmt.Sprintf("more than %d schema objects; the rest are not read", lim.MaxSchemaObjects)
			v.warns.add(at)
			return false
		}
		seen[key] = true
		obj := SchemaObject{Type: typ, Name: name, TblName: tbl, RootPage: root, Loc: r.Loc, KeyRangeViolation: r.KeyRangeViolation}
		// The statement text is kept while the total stays within its cap.
		parse := false
		switch {
		case sst != schemaSQLText:
		case total+int64(len(sql)) > lim.MaxSchemaTotalBytes:
			if !totalWarned {
				totalWarned = true
				at.Code, at.Msg = WarnLimitReached, fmt.Sprintf("schema statements exceed %d bytes in total; later ones are not kept or parsed", lim.MaxSchemaTotalBytes)
				v.warns.add(at)
			}
			sst = schemaSQLUnreadable
			total = lim.MaxSchemaTotalBytes + 1 // later statements stay over the cap
		default:
			total += int64(len(sql))
			obj.SQL = sql
			parse = len(sql) <= lim.MaxSchemaSQLBytes
			if !parse {
				at.Code, at.Msg = WarnLimitReached, fmt.Sprintf("the statement of %q is %d bytes, over the %d byte cap; it is not parsed", name, len(sql), lim.MaxSchemaSQLBytes)
				v.warns.add(at)
			}
		}
		v.parseObject(&obj, sst, parse, lim.MaxColumns, at)
		cost := int64(schemaObjectCost + len(obj.SQL) + len(name) + len(tbl))
		if obj.Table != nil {
			cost += int64(len(obj.Table.Columns)) * schemaColumnCost
		}
		if obj.Index != nil {
			cost += int64(len(obj.Index.Columns)) * 64
		}
		if err := l.alloc(cost); err != nil {
			ferr = err
			return false
		}
		sc.Objects = append(sc.Objects, obj)
		return true
	})
	if scanErr != nil {
		return nil, scanErr
	}
	if ferr != nil {
		return nil, ferr
	}
	v.sch.schema, v.sch.charge = sc, l.n
	return sc, nil
}

// parseObject fills Table, Index and Virtual of obj from its statement.
func (v *View) parseObject(obj *SchemaObject, sst sqlState, parse bool, maxCols int, at Warning) {
	unparsed := func(note string) {
		at.Code = WarnSchemaSQLUnparsed
		at.Msg = fmt.Sprintf("the definition of %q is not parsed (%s)", obj.Name, note)
		v.warns.add(at)
	}
	switch obj.Type {
	case "table":
		var def TableDef
		switch {
		case sst == schemaSQLNull:
			def = TableDef{RowidAlias: -1, ParseNote: noteEmpty}
		case !parse && sst == schemaSQLText: // over the per-statement cap
			def = TableDef{RowidAlias: -1, ParseNote: noteLimit}
		case sst == schemaSQLUnreadable:
			def = TableDef{RowidAlias: -1, ParseNote: noteLimit}
		default:
			var virtual bool
			def, virtual, _ = parseTableSQL(obj.SQL, maxCols)
			obj.Virtual = virtual
		}
		obj.Table = &def
		if !def.ParseOK {
			unparsed(def.ParseNote)
		}
	case "index":
		var def IndexDef
		switch {
		case sst == schemaSQLNull && strings.HasPrefix(asciiLower(obj.Name), "sqlite_autoindex_"):
			def = IndexDef{Table: obj.TblName, Unique: true, Auto: true}
		case sst == schemaSQLNull:
			def = IndexDef{Table: obj.TblName}
			unparsed(noteEmpty)
		case !parse:
			def = IndexDef{Table: obj.TblName}
			unparsed(noteLimit)
		default:
			def, _ = parseIndexSQL(obj.SQL, maxCols)
			if !def.ParseOK {
				def.Table = obj.TblName
				unparsed(noteSyntax)
			}
		}
		obj.Index = &def
	}
}
