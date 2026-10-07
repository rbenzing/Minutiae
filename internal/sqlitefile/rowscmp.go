package sqlitefile

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
)

// The relation of a recovered row to the live rows, computed only for a row
// whose table was proven (BasisSchema or BasisFit): by rowid for a rowid table
// and through a digest set of the live rows for a WITHOUT ROWID table. Anything
// but a clean answer is "unknown": "absent" is said only for a clean path.

type cmpKind int

const (
	cmpUnknown cmpKind = iota
	cmpSame
	cmpDiffer
	cmpAbsent
)

type cmpResult struct {
	kind cmpKind
	note string
}

// table returns the live table called name.
func (rp *rowPass) table(name string) (*Table, error) {
	if t, ok := rp.tables[name]; ok {
		return t, nil
	}
	t, err := rp.h.live.Table(rp.ctx, name)
	if err != nil {
		return nil, err
	}
	rp.tables[name] = t
	return t, nil
}

// isAnswerless reports an error that only means "the live state cannot answer":
// anything else (cancellation, the budget, an I/O error) ends the pass.
func isAnswerless(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrLimit) ||
		errors.Is(err, ErrPageUnavailable) || errors.Is(err, ErrWithoutRowid)
}

// compare relates the row to the live state.
func (rp *rowPass) compare(id ident, row *RecoveredRow) (cmpResult, error) {
	t, err := rp.table(id.table)
	if err != nil {
		if isAnswerless(err) {
			return cmpResult{kind: cmpUnknown, note: NoteLiveUncertain}, nil
		}
		return cmpResult{}, err
	}
	if id.kind == kindWithoutR {
		return rp.compareWR(t, id.table, row)
	}
	if row.Rowid == nil {
		return cmpResult{kind: cmpUnknown, note: NoteLiveUncertain}, nil
	}
	live, ok, err := t.Get(rp.ctx, *row.Rowid)
	if err != nil {
		if isAnswerless(err) {
			return cmpResult{kind: cmpUnknown, note: NoteLiveUncertain}, nil
		}
		return cmpResult{}, err
	}
	if !ok {
		return cmpResult{kind: cmpAbsent}, nil
	}
	mine := t.Resolve(Row{Rowid: *row.Rowid, HasRowid: true, Values: row.Values})
	return compareValues(mine, t.Resolve(live)), nil
}

// compareValues compares two resolved rows value by value.
func compareValues(a, b []Value) cmpResult {
	if len(a) != len(b) {
		return cmpResult{kind: cmpDiffer}
	}
	unknown := false
	for i := range a {
		same, known := sameValue(a[i], b[i])
		switch {
		case known && !same:
			return cmpResult{kind: cmpDiffer}
		case !known:
			unknown = true
		}
	}
	if unknown {
		return cmpResult{kind: cmpUnknown, note: NoteCompareIncomplete}
	}
	return cmpResult{kind: cmpSame}
}

// sameValue compares two values. known is false when an omitted or clipped
// value hides the answer.
func sameValue(a, b Value) (same, known bool) {
	if a.Kind != b.Kind {
		return false, true
	}
	switch a.Kind {
	case KindNull:
		return a.Omitted == b.Omitted, true
	case KindInt:
		return a.Int == b.Int, true
	case KindFloat:
		return math.Float64bits(a.Float) == math.Float64bits(b.Float), true
	}
	if a.Len != b.Len {
		return false, true
	}
	if a.Omitted || b.Omitted || a.Clipped || b.Clipped {
		return false, false
	}
	return string(a.Bytes) == string(b.Bytes), true
}

// ---- WITHOUT ROWID: the digest set of the live rows ----

// diffEntryCost is what one entry of a digest set costs in the budget: two
// digests, the map slot and its overhead.
const diffEntryCost = 160

type wrEntry struct {
	digest   [32]byte
	complete bool // the live row held every value: its digest is the row's
}

// wrSet is the digest set of the live rows of one WITHOUT ROWID table.
type wrSet struct {
	byKey   map[[32]byte]wrEntry // primary-key digest (row digest when pkOK is false)
	pk      []int                // positions of the primary-key columns in a resolved row
	pkOK    bool                 // every key column compares as BINARY: equal keys are equal bytes
	tainted bool                 // the live scan met damage or an omitted value: it may be incomplete
	capped  bool                 // a cap was reached: the set holds nothing
	charged int64
}

// digestOf hashes values; complete is false when one is omitted or clipped.
func digestOf(vals []Value, idx []int) (d [32]byte, complete bool) {
	h := sha256.New()
	complete = true
	var b [8]byte
	put := func(n uint64) {
		binary.BigEndian.PutUint64(b[:], n)
		h.Write(b[:])
	}
	one := func(v Value) {
		h.Write([]byte{byte(v.Kind)})
		switch v.Kind {
		case KindInt:
			put(uint64(v.Int))
		case KindFloat:
			put(math.Float64bits(v.Float))
		case KindText, KindBlob:
			put(uint64(v.Len))
			h.Write(v.Bytes)
		}
		if v.Omitted || v.Clipped {
			complete = false
		}
	}
	if idx == nil {
		for _, v := range vals {
			one(v)
		}
	} else {
		for _, i := range idx {
			one(vals[i])
		}
	}
	copy(d[:], h.Sum(nil))
	return d, complete
}

// wrSetOf builds (once per table) the digest set of the live rows.
func (rp *rowPass) wrSetOf(ctx context.Context, t *Table, name string) (*wrSet, error) {
	if s, ok := rp.wr[name]; ok {
		return s, nil
	}
	s := &wrSet{byKey: map[[32]byte]wrEntry{}, pkOK: true}
	rp.wr[name] = s
	def := t.Def()
	type pkc struct{ ord, pos int }
	var pks []pkc
	for i := range def.Columns {
		c := &def.Columns[i]
		if c.PKOrdinal > 0 {
			pks = append(pks, pkc{c.PKOrdinal, i})
			if c.Collation != "" && !asciiEqualFold(c.Collation, "BINARY") {
				s.pkOK = false
			}
		}
	}
	slices.SortFunc(pks, func(a, b pkc) int { return a.ord - b.ord })
	for _, p := range pks {
		s.pk = append(s.pk, p.pos)
	}
	if len(s.pk) == 0 {
		s.pkOK = false
	}
	lim := rp.h.d.env.opts.Limits
	var cerr error
	err := t.Rows(ctx, func(r Row) bool {
		if int64(len(s.byKey)) >= lim.MaxDiffRows {
			s.capped = true
			rp.limitHit("MaxDiffRows", fmt.Sprintf("the live rows of table %q are more than Limits.MaxDiffRows (%d): its history rows are not compared", name, lim.MaxDiffRows))
			return false
		}
		if rp.diffTotal >= lim.MaxDiffRowsTotal {
			s.capped = true
			rp.limitHit("MaxDiffRowsTotal", fmt.Sprintf("the live rows compared so far are more than Limits.MaxDiffRowsTotal (%d): the rest of the history rows are not compared", lim.MaxDiffRowsTotal))
			return false
		}
		if cerr = rp.l.alloc(diffEntryCost); cerr != nil {
			return false
		}
		s.charged += diffEntryCost
		rp.diffTotal++
		res := t.Resolve(r)
		rowD, complete := digestOf(res, nil)
		if r.KeyRangeViolation {
			s.tainted = true
		}
		key := rowD
		if s.pkOK {
			key, _ = digestOf(res, s.pk)
		}
		s.byKey[key] = wrEntry{digest: rowD, complete: complete}
		if !complete {
			s.tainted = true
		}
		return true
	})
	if cerr != nil {
		return nil, cerr
	}
	if err != nil {
		if !isAnswerless(err) {
			return nil, err
		}
		s.tainted = true
	}
	if liveDamaged(rp.h.live) {
		s.tainted = true
	}
	if s.capped {
		rp.l.free(s.charged)
		s.charged = 0
		s.byKey = nil
	}
	return s, nil
}

// compareWR relates a row of a WITHOUT ROWID table to the digest set.
func (rp *rowPass) compareWR(t *Table, name string, row *RecoveredRow) (cmpResult, error) {
	s, err := rp.wrSetOf(rp.ctx, t, name)
	if err != nil {
		return cmpResult{}, err
	}
	if s.capped {
		return cmpResult{kind: cmpUnknown, note: "limit-reached"}, nil
	}
	res := t.Resolve(Row{Values: row.Values})
	rowD, complete := digestOf(res, nil)
	key := rowD
	if s.pkOK {
		var pkDone bool
		key, pkDone = digestOf(res, s.pk)
		if !pkDone {
			return cmpResult{kind: cmpUnknown, note: NoteCompareIncomplete}, nil
		}
	} else if !complete {
		return cmpResult{kind: cmpUnknown, note: NoteCompareIncomplete}, nil
	}
	e, found := s.byKey[key]
	switch {
	case !found && s.tainted, !found && !s.pkOK:
		return cmpResult{kind: cmpUnknown, note: NoteLiveUncertain}, nil
	case !found:
		return cmpResult{kind: cmpAbsent}, nil
	case !e.complete || !complete:
		return cmpResult{kind: cmpUnknown, note: NoteCompareIncomplete}, nil
	case e.digest == rowD:
		return cmpResult{kind: cmpSame}, nil
	}
	return cmpResult{kind: cmpDiffer}, nil
}

// liveDamaged reports whether the live view met structural damage (a page it
// could not use, a cell or record it could not read): a digest set built from
// such a view may miss rows, so it can prove a row equal but never absent.
func liveDamaged(v *View) bool {
	for _, w := range v.Warnings() {
		switch w.Code {
		case WarnPageUnavailable, WarnPageTypeInvalid, WarnPageRange, WarnCellPointer, WarnCellOverflowChain,
			WarnCellTooLarge, WarnRecordInvalid, WarnBTreeCycle, WarnBTreeDepth, WarnBTreeOrder, WarnBTreeShape,
			WarnSchemaRowInvalid, WarnLimitReached:
			return true
		}
	}
	return false
}
