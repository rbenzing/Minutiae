package sqlitedb

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// KeyOf builds the join key of column col of r from the SOURCE column: the
// value, the column's canonical collation (an unsupported one is kept as
// written, so a join refuses it) and its affinity class. It is false for NULL,
// an absent column, an unknown state and NaN.
func KeyOf(r Row, col int) (parse.JoinKey, bool) {
	if r.tbl == nil || col < 0 || col >= len(r.vals) {
		return parse.JoinKey{}, false
	}
	var k parse.JoinKey
	switch v, ok := r.known(col); {
	case !ok:
		return parse.JoinKey{}, false
	case v.Kind == sqlitefile.KindInt:
		k = parse.JoinKey{Kind: parse.JoinInt, I: v.Int}
	case v.Kind == sqlitefile.KindFloat:
		if math.IsNaN(v.Float) {
			return parse.JoinKey{}, false
		}
		k = parse.JoinKey{Kind: parse.JoinFloat, F: math.Float64bits(v.Float)}
	case v.Kind == sqlitefile.KindText:
		s, ok := r.Text(col)
		if !ok {
			return parse.JoinKey{}, false
		}
		k = parse.JoinKey{Kind: parse.JoinText, S: string(s)}
	case v.Kind == sqlitefile.KindBlob:
		k = parse.JoinKey{Kind: parse.JoinBlob, S: string(v.Bytes)}
	default:
		return parse.JoinKey{}, false
	}
	if col >= len(r.tbl.def.Columns) {
		return parse.JoinKey{}, false
	}
	c := r.tbl.def.Columns[col]
	k.Collation = sqlitefile.ColumnCollation(c)
	if canon, err := sqlitefile.CanonicalCollation(k.Collation); err == nil {
		k.Collation = canon
	}
	k.Class = classOf(c.Affinity)
	return k, true
}

func classOf(a sqlitefile.Affinity) parse.JoinClass {
	switch a {
	case sqlitefile.AffInteger, sqlitefile.AffReal, sqlitefile.AffNumeric:
		return parse.ClassNumeric
	case sqlitefile.AffText:
		return parse.ClassText
	}
	return parse.ClassBlob
}

// LiteralIntKey is the key of an integer constant (never flagged).
func LiteralIntKey(v int64) parse.JoinKey { return parse.JoinKey{Kind: parse.JoinInt, I: v} }

// LiteralFloatKey is the key of a real constant; false for NaN, which equals
// nothing.
func LiteralFloatKey(f float64) (parse.JoinKey, bool) {
	if math.IsNaN(f) {
		return parse.JoinKey{}, false
	}
	return parse.JoinKey{Kind: parse.JoinFloat, F: math.Float64bits(f)}, true
}

// LiteralTextKey is the key of a text constant, the bytes as given.
func LiteralTextKey(b []byte) parse.JoinKey { return parse.JoinKey{Kind: parse.JoinText, S: string(b)} }

// LiteralBlobKey is the key of a blob constant.
func LiteralBlobKey(b []byte) parse.JoinKey { return parse.JoinKey{Kind: parse.JoinBlob, S: string(b)} }

// JoinFlags are the facts about one lookup that the engine's own comparison
// might treat differently from this layer.
type JoinFlags uint8

// The join flags.
const (
	JoinCollationDiffers JoinFlags = 1 << iota // the source column's collation differs from the target's
	JoinAffinityDiffers                        // the source column's affinity class differs from the target's
)

const (
	// indexEntryCost is a constant upper bound of one map entry and its slice
	// header; 32 more bytes are charged for the key.
	indexEntryCost    = 64
	defaultIndexLimit = 1_000_000
)

// Index maps the values of one TARGET column to rowids under that column's
// collation. It is NOT safe for concurrent use. Its memory is charged to the
// budget until Release or DB.Release.
type Index struct {
	db        *DB
	table     string
	column    string
	collation string // canonical
	class     parse.JoinClass
	m         map[[32]byte][]int64
	unkeyed   int
	held      int64
}

// Index reads the table once and indexes column col under the column's own
// collation. NULL, NaN and values in an unknown state are not indexed and are
// counted (Unkeyed, Stats.IndexUnkeyed). More than limit entries (limit <= 0 means
// 1,000,000) is ErrIndexLimit; nothing is kept on any error. An unsupported
// collation is an *UnsupportedCollationError, never BINARY; a WITHOUT ROWID
// table is ErrWithoutRowid. No affinity conversion is done.
func (t *Table) Index(ctx context.Context, col string, limit int) (ix *Index, err error) {
	defer func() {
		if r := recover(); r != nil {
			ix, err = nil, fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	db := t.db
	if db.released {
		return nil, ErrReleased
	}
	if t.def.WithoutRowid {
		return nil, fmt.Errorf("%w: %q", ErrWithoutRowid, t.name)
	}
	ci := t.Col(col)
	if ci < 0 {
		return nil, &UnsupportedSchemaError{Table: t.name, Missing: []string{col}}
	}
	info := t.Cols()[ci]
	coll, cerr := sqlitefile.CanonicalCollation(info.Collation)
	if cerr != nil {
		return nil, &UnsupportedCollationError{Table: t.name, Column: info.Name, Collation: info.Collation}
	}
	if limit <= 0 {
		limit = defaultIndexLimit
	}
	ix = &Index{
		db: db, table: t.name, column: info.Name, collation: coll, class: classOf(info.Affinity),
		m: map[[32]byte][]int64{},
	}
	entries := 0
	err = t.Scan(ctx, func(r Row) error {
		rowid, _ := r.Rowid()
		v, ok := r.known(ci)
		if !ok {
			ix.unkeyed++
			return nil
		}
		if v.Kind == sqlitefile.KindText && isUTF16(v.Enc) { // compare the decoded text in every collation
			s, ok := r.Text(ci)
			if !ok {
				ix.unkeyed++
				return nil
			}
			v = sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: s, Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
		}
		key, st, kerr := sqlitefile.EqualityKey(v, coll)
		if kerr != nil {
			return kerr
		}
		if st != sqlitefile.KeyOK {
			ix.unkeyed++
			return nil
		}
		if entries >= limit {
			return ErrIndexLimit
		}
		if err := db.budget.Alloc(indexEntryCost + 32); err != nil {
			return err
		}
		ix.held += indexEntryCost + 32
		entries++
		ix.m[key] = append(ix.m[key], rowid)
		return nil
	})
	if err != nil {
		ix.Release()
		return nil, err
	}
	db.stats.IndexUnkeyed += int64(ix.unkeyed)
	return ix, nil
}

// Unkeyed is the number of target rows that are not in the index (NULL, NaN,
// unknown state).
func (ix *Index) Unkeyed() int { return ix.unkeyed }

// Release returns the index's memory to the budget and empties it. It is
// idempotent.
func (ix *Index) Release() {
	if ix == nil {
		return
	}
	if ix.held > 0 {
		ix.db.budget.Free(ix.held)
		ix.held = 0
	}
	ix.m = nil
}

// Rowids returns, in ascending order, the rowids whose indexed value equals
// key under the target column's collation, and the flags of a lookup whose
// source column's collation or affinity class differs from the target's. A
// JoinNone key matches nothing; so does a key with no engine equality (NaN) or
// a text the reader cannot fold under the target collation (invalid UTF-8 under
// NOCASE or RTRIM). A source collation that is not supported is an
// *UnsupportedCollationError. The slice is the caller's.
func (ix *Index) Rowids(key parse.JoinKey) (rowids []int64, flags JoinFlags, err error) {
	if key.Kind == parse.JoinNone {
		return nil, 0, nil
	}
	if ix.m == nil {
		return nil, 0, ErrReleased
	}
	if key.Collation != "" {
		canon, cerr := sqlitefile.CanonicalCollation(key.Collation)
		if cerr != nil {
			return nil, 0, &UnsupportedCollationError{Table: ix.table, Column: "(join source)", Collation: key.Collation}
		}
		if canon != ix.collation {
			flags |= JoinCollationDiffers
		}
	}
	if key.Class != parse.ClassUnknown && key.Class != ix.class {
		flags |= JoinAffinityDiffers
	}
	var v sqlitefile.Value
	switch key.Kind {
	case parse.JoinInt:
		v = sqlitefile.Value{Kind: sqlitefile.KindInt, Int: key.I}
	case parse.JoinFloat:
		v = sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Float64frombits(key.F)}
	case parse.JoinText:
		v = sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(key.S), Len: int64(len(key.S)), Enc: sqlitefile.EncUTF8}
	case parse.JoinBlob:
		v = sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte(key.S), Len: int64(len(key.S))}
	default:
		return nil, flags, nil
	}
	k, st, kerr := sqlitefile.EqualityKey(v, ix.collation)
	if kerr != nil {
		return nil, 0, kerr
	}
	if st != sqlitefile.KeyOK {
		return nil, flags, nil
	}
	return slices.Clone(ix.m[k]), flags, nil
}
