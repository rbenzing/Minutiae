package sqlitefile_test

import (
	"context"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestRecordInvalidMeansTheRowWasNotRead: record-invalid is raised exactly for
// a cell that yields no row, never for a row that was delivered. A record that
// holds a reserved serial type is delivered (read as NULL, as the engine reads
// it) and raises record-reserved-serial instead. The scan and the point lookup
// follow the same rule.
func TestRecordInvalidMeansTheRowWasNotRead(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
	tb.Insert(1, "one", int64(1))
	tb.InsertRaw(2, []uint64{10, 1}, []byte{7})        // a reserved serial type: read as NULL, row kept
	tb.InsertRaw(3, []uint64{1, 13 + 2*40}, []byte{9}) // declares 40 text bytes it lacks: row not read
	tb.InsertRaw(4, []uint64{11, 11, 1}, []byte{5})    // two reserved types in one record: one warning
	data := b.Bytes()
	off := func(rowid int64) int64 {
		_, pg, o := tb.CellBytes(rowid)
		return int64(pg-1)*512 + int64(o)
	}
	offsets := func(v *sqlitefile.View, code string) []int64 {
		var out []int64
		for _, w := range v.Warnings() {
			if w.Code == code {
				out = append(out, w.Offset)
			}
		}
		slices.Sort(out)
		return out
	}

	t.Run("scan", func(t *testing.T) {
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		if got := rowids(scanRows(t, v, tb.Root(), sqlitefile.TableTree)); !slices.Equal(got, []int64{1, 2, 4}) {
			t.Fatalf("rowids %v, want [1 2 4]", got)
		}
		if got := offsets(v, sqlitefile.WarnRecordInvalid); !slices.Equal(got, []int64{off(3)}) {
			t.Errorf("record-invalid at %v, want only the row not read, at %d", got, off(3))
		}
		if got := offsets(v, sqlitefile.WarnRecordReservedSerial); !slices.Equal(got, sorted(off(2), off(4))) {
			t.Errorf("record-reserved-serial at %v, want one per delivered row: %d, %d", got, off(2), off(4))
		}
	})
	t.Run("get", func(t *testing.T) {
		for _, id := range []int64{2, 4} {
			_, v := openLive(t, data, sqlitefile.Options{})
			row, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
			if err != nil || !ok || row.Rowid != id {
				t.Fatalf("Get %d: ok %v err %v", id, ok, err)
			}
			if got := offsets(v, sqlitefile.WarnRecordInvalid); len(got) != 0 {
				t.Errorf("Get %d: a delivered row carries record-invalid: %v", id, v.Warnings())
			}
			if got := offsets(v, sqlitefile.WarnRecordReservedSerial); !slices.Equal(got, []int64{off(id)}) {
				t.Errorf("Get %d: record-reserved-serial at %v: %v", id, got, v.Warnings())
			}
			v.Release()
		}
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		if _, ok, err := v.LookupRowid(context.Background(), tb.Root(), 3); err != nil || ok {
			t.Fatalf("Get 3: ok %v err %v, want not found", ok, err)
		}
		if got := offsets(v, sqlitefile.WarnRecordInvalid); !slices.Equal(got, []int64{off(3)}) {
			t.Errorf("Get 3: record-invalid at %v, want [%d]: %v", got, off(3), v.Warnings())
		}
	})
}

func sorted(v ...int64) []int64 { slices.Sort(v); return v }
