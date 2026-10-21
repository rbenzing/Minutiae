package sqlitefile_test

// The identity of history rows whose record is truncated or holds a reserved serial type (LX-4
// Task 11, L108 item 4). The page is a freelist page: the schema does not own it, so its cells
// are named by shape only. (A page reachable from the schema is the table's by reachability, and a
// truncated cell does not change the owner.)

import (
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// freelistRow places the page of a donor table big(a, b) on a freelist leaf of a database whose
// only table is t(a, b), and returns the one freelist row.
func freelistRow(t *testing.T, donorPage func(t *testing.T) []byte) sqlitefile.RecoveredRow {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	b.SetFreelist([][]uint32{{3, 4}})
	db := b.Bytes()
	copy(db[3*hps:], donorPage(t)) // page 4
	_, h := openAll(t, db, nil, nil)
	rows, _ := collectRows(t, h)
	return rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
}

func plainDonor(_ *testing.T) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("big", "create table big(a, b)")
	tb.Insert(1, int64(5), "text")
	return b.Snapshot().Page(2)
}

func truncatedDonor(t *testing.T) []byte {
	donor := newBigScen(t, hps)
	page := donor.v0.Page(2)
	cell, _, off := donor.tb.CellBytes(1)
	binary.BigEndian.PutUint32(page[off+len(cell)-4:], 3) // the chain starts at page 3: a live page here
	return page
}

func reservedDonor(_ *testing.T) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("big", "create table big(a, b)")
	tb.InsertRaw(1, []uint64{1, 10}, []byte{5}) // b has the reserved serial type 10: read as NULL
	return b.Snapshot().Page(2)
}

func TestHistoryRowOfATruncatedRecordIsNotNamedByAStrictFit(t *testing.T) {
	r := freelistRow(t, truncatedDonor)
	if !r.Truncated {
		t.Fatalf("the row is not truncated: %+v", r)
	}
	if r.Table != "t" || r.TableBasis != sqlitefile.BasisGuess || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
		t.Errorf("truncated row: table %q basis %s relation %s notes %v, want t, guess, unknown, identity-by-fit-only", r.Table, r.TableBasis, r.Relation, r.Notes)
	}
}

func TestHistoryRowOfAReservedSerialRecordIsNotNamedByAStrictFit(t *testing.T) {
	r := freelistRow(t, reservedDonor)
	if r.Truncated || r.Values[1].Serial != 10 {
		t.Fatalf("the row does not hold a reserved serial type: %+v", r)
	}
	if r.Table != "t" || r.TableBasis != sqlitefile.BasisGuess || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
		t.Errorf("reserved row: table %q basis %s relation %s notes %v, want t, guess, unknown, identity-by-fit-only", r.Table, r.TableBasis, r.Relation, r.Notes)
	}
}

// The false-positive side: the same shape, written whole by the engine's rules, still fits strictly.
func TestHistoryRowOfAPlainRecordStillFitsStrictly(t *testing.T) {
	r := freelistRow(t, plainDonor)
	if r.Table != "t" || r.TableBasis != sqlitefile.BasisFit || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
		t.Errorf("plain row: table %q basis %s relation %s notes %v, want t, fit, unknown, identity-by-fit-only", r.Table, r.TableBasis, r.Relation, r.Notes)
	}
}
