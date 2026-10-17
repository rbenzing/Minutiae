package sqlitefile_test

// FitCells (LX-4 Task 11, L108 item 4): a cell whose record is truncated or holds a reserved
// serial type cannot support a strict identity.

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func fitCellsSchema() *sqlitefile.Schema {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	return schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I), fcol("b", T), fcol("c", T))))
}

func plainCells(vals ...[]sqlitefile.Value) []sqlitefile.FitCell {
	out := make([]sqlitefile.FitCell, len(vals))
	for i, v := range vals {
		out[i] = sqlitefile.FitCell{Values: v}
	}
	return out
}

func TestFitCellsTruncatedCellNeverFitsStrictly(t *testing.T) {
	s := fitCellsSchema()
	short := []sqlitefile.Value{vInt(1), vText("x")} // c is missing: an ALTER-added column when the record is whole
	full := []sqlitefile.Value{vInt(2), vText("y"), vText("z")}
	if tb, basis := s.FitCells(plainCells(short, full)); tb != "t" || basis != sqlitefile.BasisFit {
		t.Fatalf("plain cells: %q %s, want t fit", tb, basis)
	}
	cells := plainCells(short, full)
	cells[0].Truncated = true
	if tb, basis := s.FitCells(cells); tb != "t" || basis != sqlitefile.BasisGuess {
		t.Errorf("a truncated cell: %q %s, want t guess", tb, basis)
	}
	// the strict tier is denied for the whole page, whichever cell is the truncated one
	cells = plainCells(short, full)
	cells[1].Truncated = true
	if tb, basis := s.FitCells(cells); tb != "t" || basis != sqlitefile.BasisGuess {
		t.Errorf("a truncated second cell: %q %s, want t guess", tb, basis)
	}
}

func TestFitCellsReservedSerialCellNeverFitsStrictly(t *testing.T) {
	s := fitCellsSchema()
	cell := []sqlitefile.Value{vInt(1), vNull(), vText("z")} // b read from a reserved serial type
	cells := plainCells(cell)
	if tb, basis := s.FitCells(cells); tb != "t" || basis != sqlitefile.BasisFit {
		t.Fatalf("plain cell: %q %s, want t fit", tb, basis)
	}
	cells[0].Reserved = 1
	if tb, basis := s.FitCells(cells); tb != "t" || basis != sqlitefile.BasisGuess {
		t.Errorf("a reserved-serial cell: %q %s, want t guess", tb, basis)
	}
	tb, basis, limited := s.FitCellsDetail(cells)
	if tb != "t" || basis != sqlitefile.BasisGuess || limited {
		t.Errorf("FitCellsDetail: %q %s limited %v", tb, basis, limited)
	}
}

// A cell that cannot even fit loosely stays unnamed, flagged or not.
func TestFitCellsFlaggedCellStillNeedsALooseFit(t *testing.T) {
	s := fitCellsSchema()
	cells := plainCells([]sqlitefile.Value{vText("x"), vInt(1), vInt(2), vInt(3)})
	cells[0].Truncated = true
	if tb, basis := s.FitCells(cells); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("%q %s, want none", tb, basis)
	}
}

// Plain cells give the same answer through FitPage and FitCells: the strict tier is not lost for
// engine-written records.
func TestFitCellsPlainEqualsFitPage(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	schemas := []*sqlitefile.Schema{
		fitCellsSchema(),
		schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I), notNull(fcol("b", T)))), tableObj("u", tableDef(false, -1, fcol("x", T), fcol("y", I)))),
		schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I))), tableObj("u", tableDef(false, -1, fcol("a", I)))),
	}
	pages := [][][]sqlitefile.Value{
		{{vInt(1), vText("x"), vText("z")}},
		{{vInt(1), vText("x")}, {vInt(2), vText("y")}},
		{{vInt(1)}},
		{{vText("x"), vInt(1)}},
		{{vInt(1), vNull(), vNull()}, {vInt(2)}},
		{{vInt(1), vInt(2), vInt(3)}},
		{{vNull()}, {vNull(), vNull()}},
		{},
		{{vBlob(1), vBlob(2)}},
	}
	n := 0
	for si, s := range schemas {
		for pi, page := range pages {
			cells := make([]sqlitefile.FitCell, len(page))
			for i, v := range page {
				cells[i] = sqlitefile.FitCell{Values: v}
			}
			wt, wb := s.FitPage(page)
			gt, gb := s.FitCells(cells)
			if gt != wt || gb != wb {
				t.Errorf("schema %d page %d: FitCells %q %s, FitPage %q %s", si, pi, gt, gb, wt, wb)
			}
			n++
		}
	}
	if n < 20 {
		t.Fatalf("%d shapes", n)
	}
}
