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
// written, so a join refuses it) and its affinity class.
//
// It is (zero, false, nil) only for a value the engine matches with nothing:
// NULL and NaN (the engine reads a stored NaN as NULL). A value this layer
// cannot decide (an absent or generated column, an omitted, unread, clipped,
// lost or undecodable value, a kind it does not know) is an error wrapping
// ErrKeyUndecidable: a mapper that skipped it would turn an unknown into a
// silent non-match.
func KeyOf(r Row, col int) (parse.JoinKey, bool, error) {
	if r.tbl == nil || col < 0 || col >= len(r.vals) || col >= len(r.tbl.def.Columns) {
		return parse.JoinKey{}, false, fmt.Errorf("%w: no such column %d", ErrKeyUndecidable, col)
	}
	st := r.State(col)
	switch st {
	case StateNull:
		return parse.JoinKey{}, false, nil
	case StatePresent, StateDefaulted:
	default:
		return parse.JoinKey{}, false, fmt.Errorf("%w: column %d is %s", ErrKeyUndecidable, col, st)
	}
	v := r.vals[col]
	var k parse.JoinKey
	switch v.Kind {
	case sqlitefile.KindNull:
		return parse.JoinKey{}, false, nil
	case sqlitefile.KindInt:
		k = parse.JoinKey{Kind: parse.JoinInt, I: v.Int}
	case sqlitefile.KindFloat:
		if math.IsNaN(v.Float) {
			return parse.JoinKey{}, false, nil
		}
		k = parse.JoinKey{Kind: parse.JoinFloat, F: math.Float64bits(v.Float)}
	case sqlitefile.KindText:
		s, ok := r.Text(col)
		if !ok {
			return parse.JoinKey{}, false, fmt.Errorf("%w: text of column %d is not available", ErrKeyUndecidable, col)
		}
		k = parse.JoinKey{Kind: parse.JoinText, S: string(s)}
	case sqlitefile.KindBlob:
		k = parse.JoinKey{Kind: parse.JoinBlob, S: string(v.Bytes)}
	default:
		return parse.JoinKey{}, false, fmt.Errorf("%w: column %d holds a kind this layer does not know", ErrKeyUndecidable, col)
	}
	c := r.tbl.def.Columns[col]
	k.Collation = sqlitefile.ColumnCollation(c)
	if canon, err := sqlitefile.CanonicalCollation(k.Collation); err == nil {
		k.Collation = canon
	}
	k.Class = classOf(c.Affinity)
	return k, true, nil
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

// LiteralTextKey is the key of a text constant, the bytes as given. A literal is
// never flagged and no affinity is applied to it, so against an INTEGER or REAL
// column it does not match "7" with 7 as the engine does for a constant
// (documented gap, T8 M2).
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
	// JoinUndecided marks a HIT returned by an index that could not decide every
	// target row (a row whose key is unknown, or rows the scan lost): the answer
	// may be short, so it is never presented as the complete set of matches.
	JoinUndecided
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
	unkeyed   int                 // target rows not in the index (NULL, NaN, undecided)
	undecided int                 // of those, rows whose key is unknown: a miss cannot be proven
	lost      sqlitefile.ScanLoss // what the scan that built the index did not deliver
	ch        charger             // the memory of the entries
}

// Index reads the table once and indexes column col under the column's own
// collation. NULL, NaN and values in an unknown state are not indexed and are
// counted (Unkeyed, Stats.IndexUnkeyed). More than limit entries (limit <= 0 means
// 1,000,000) is ErrIndexLimit; nothing is kept on any error. An unsupported
// collation is an *UnsupportedCollationError, never BINARY; a WITHOUT ROWID
// table is ErrWithoutRowid. No affinity conversion is done.
//
// An index built from a scan that lost rows (a skipped page or cell, counted by
// the reader per scan) is Lossy: every miss of Rowids is then an
// ErrKeyUndecidable naming the loss, and a hit is flagged JoinUndecided.
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
		m: map[[32]byte][]int64{}, ch: charger{db: db},
	}
	entries := 0
	ix.lost, err = t.scan(ctx, func(r Row) error {
		rowid, _ := r.Rowid()
		switch r.State(ci) {
		case StateNull:
			ix.unkeyed++ // decided: the engine matches NULL with nothing
			return nil
		case StatePresent, StateDefaulted:
		default:
			ix.unkeyed++
			ix.undecided++
			return nil
		}
		v := r.vals[ci]
		if v.Kind == sqlitefile.KindText && isUTF16(v.Enc) { // compare the decoded text in every collation
			s, ok := r.Text(ci)
			if !ok { // only text that decodes exactly is keyed: U+FFFD would join different values
				ix.unkeyed++
				ix.undecided++
				return nil
			}
			v = sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: s, Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
		}
		key, st, kerr := sqlitefile.EqualityKey(v, coll)
		if kerr != nil {
			return fmt.Errorf("%w: %w", ErrInternal, kerr)
		}
		if st != sqlitefile.KeyOK {
			ix.unkeyed++
			if st == sqlitefile.KeyUnknown {
				ix.undecided++
			}
			return nil
		}
		if entries >= limit {
			return ErrIndexLimit
		}
		if err := ix.ch.charge(indexEntryCost + 32); err != nil {
			return err
		}
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

// Undecided is the number of target rows whose key is unknown (an unread,
// omitted, clipped or undecodable value, or text the reader cannot fold). They
// make every miss of Rowids an ErrKeyUndecidable.
func (ix *Index) Undecided() int { return ix.undecided }

// Lossy reports whether the scan that built the index lost rows (the reader
// skipped a damaged page or cell). A lossy index cannot prove any miss.
func (ix *Index) Lossy() bool { return ix.lost.Any() }

// Release returns the index's memory to the budget and empties it. It is
// idempotent.
func (ix *Index) Release() {
	if ix == nil {
		return
	}
	ix.ch.release()
	ix.m = nil
}

// Rowids returns, in ascending order, the rowids whose indexed value equals
// key under the target column's collation, and the flags of a lookup whose
// source column's collation or affinity class differs from the target's. A
// JoinNone key matches nothing, and so does a NaN (the engine reads it as NULL).
// A probe the reader cannot compare (invalid UTF-8 under NOCASE or RTRIM, a
// kind it does not know) is an error wrapping ErrKeyUndecidable, and so is a
// lookup that finds no row while Undecided target rows exist or the index is
// Lossy: a miss is not a proof of absence. A hit is returned, with JoinUndecided
// set in the flags in those two cases: the set of matches may be short. A
// source collation that is not supported is an *UnsupportedCollationError. The
// slice is the caller's.
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
		return nil, flags, fmt.Errorf("%w: unknown key kind %d", ErrKeyUndecidable, key.Kind)
	}
	k, st, kerr := sqlitefile.EqualityKey(v, ix.collation)
	if kerr != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrInternal, kerr)
	}
	switch st {
	case sqlitefile.KeyOK:
	case sqlitefile.KeyNever:
		return nil, flags, nil
	default:
		return nil, flags, fmt.Errorf("%w: the probe cannot be compared under %s", ErrKeyUndecidable, ix.collation)
	}
	if ids := ix.m[k]; len(ids) > 0 {
		if ix.undecided > 0 || ix.lost.Any() {
			flags |= JoinUndecided
		}
		return slices.Clone(ids), flags, nil
	}
	if ix.lost.Any() {
		return nil, flags, fmt.Errorf("%w: no row matches, but the scan of %s.%s lost rows (%d pages, %d cells skipped as damaged)", ErrKeyUndecidable, ix.table, ix.column, ix.lost.Pages, ix.lost.Cells)
	}
	if ix.undecided > 0 {
		return nil, flags, fmt.Errorf("%w: no row matches, but %d rows of %s.%s have keys that could not be decided", ErrKeyUndecidable, ix.undecided, ix.table, ix.column)
	}
	return nil, flags, nil
}
