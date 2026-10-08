package sqlitefile

import "slices"

// PagePart is one overflow page the bytes of a value came from.
type PagePart struct {
	Page uint32
	At   PageLoc
}

// Loc says where the cell of a row lies on disk. Offset is the first byte of
// the cell in File (on page 1, the page's own offset 0 is the file's offset 0,
// so cell offsets are relative to the page start as the format says).
type Loc struct {
	File       FileKind
	Page       uint32
	Cell       int   // index in the cell pointer array
	Offset     int64 // first byte of the cell in File
	Length     int64 // cell length in File: header + local payload + overflow pointer
	PageOffset int64 // start of the page image in File (Offset-PageOffset = offset in page)
	Frame      uint32
	Record     int
	// Overflow lists the overflow pages the row's value bytes came from, in
	// chain order (the first Limits.MaxLocOverflow). A value that was omitted
	// (over a cap) was not read, so its pages are not listed.
	Overflow []PagePart
	// OverflowTotal counts every overflow page the row's bytes came from,
	// listed or not.
	OverflowTotal int
	// OverflowMixed: some overflow page lies in a different file, WAL frame or
	// journal record than the cell.
	OverflowMixed bool
}

// Row is one cell of a b-tree, decoded.
type Row struct {
	Rowid      int64
	HasRowid   bool    // false for index entries and WITHOUT ROWID rows
	Values     []Value // as stored in the record
	Loc        Loc
	PayloadLen int64
	// KeyRangeViolation is set on a table row whose rowid lies outside the key
	// range its position in the tree promises (the separator keys of its
	// ancestors): a doctored or damaged interior key. The row is delivered as
	// the engine walk delivers it, and a btree-order warning is raised.
	KeyRangeViolation bool
	// LengthMismatch: the row's record is longer than its header and body
	// account for, or has no columns (see Record.LengthMismatch); the values are
	// delivered and a record-length-mismatch warning is raised.
	LengthMismatch bool
}

// Clone returns a copy that owns all its memory and stays valid after the
// visit callback it came from returns.
func (r Row) Clone() Row {
	out := r
	out.Values = slices.Clone(r.Values)
	for i := range out.Values {
		if out.Values[i].Bytes != nil {
			out.Values[i].Bytes = slices.Clone(r.Values[i].Bytes)
		}
	}
	out.Loc.Overflow = slices.Clone(r.Loc.Overflow)
	return out
}

// newLoc builds the location of cell c, at pointer index idx of the page whose
// image is at at, from the overflow pages its payload followed.
func newLoc(at PageLoc, pgno uint32, idx int, c Cell, steps []chainStep, maxOverflow int) Loc {
	l := Loc{
		File: at.File, Page: pgno, Cell: idx,
		Offset: at.Offset + int64(c.Offset), Length: int64(c.Length), PageOffset: at.Offset,
		Frame: at.Frame, Record: at.Record, OverflowTotal: len(steps),
	}
	for i, s := range steps {
		if i < maxOverflow {
			l.Overflow = append(l.Overflow, PagePart(s))
		}
		if s.At.File != at.File || s.At.Frame != at.Frame || s.At.Record != at.Record {
			l.OverflowMixed = true
		}
	}
	return l
}
