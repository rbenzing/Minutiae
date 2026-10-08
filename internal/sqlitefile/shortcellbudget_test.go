package sqlitefile_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestShortCellsDoNotLeakBudget: a history cell whose record declares more
// bytes than the cell holds is skipped, and the charge its header took is given
// back at once. The peak charge of a pass over N copies of such a page does not
// grow with N (final review B, I-3).
func TestShortCellsDoNotLeakBudget(t *testing.T) {
	peak := func(n int, short bool) (int64, int64) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		tb := b.CreateTable("t", "create table t(a, b)")
		for id := int64(1); id <= 9; id++ {
			// 40 columns: the header charge is large, the record declares a text of
			// 1000 bytes that the cell does not hold
			serials := make([]uint64, 40)
			for i := range serials {
				serials[i] = 1
			}
			body := make([]byte, 39+3)
			if !short {
				serials[39] = 13 + 2*3
			} else {
				serials[39] = 13 + 2*1000
			}
			tb.InsertRaw(id, serials, body)
		}
		snap := b.Snapshot()
		db := withCount(b.Bytes(), snap.Pages())
		j := b.NewJournal(hps, 0x1234, snap.Pages())
		for range n {
			j.Record(2, snap.Page(2))
		}
		pb := &peakBudget{}
		d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Budget: pb})
		if err != nil {
			t.Fatal(err)
		}
		jb := j.Bytes()
		if _, err := d.AttachJournal(bytes.NewReader(jb), int64(len(jb))); err != nil {
			t.Fatal(err)
		}
		h := d.History()
		rows, st := collectRows(t, h)
		if short && (len(rows) != 0 || st.CellsShort < int64(9*n)) {
			t.Fatalf("%d rows, CellsShort %d: the cells must be short and skipped", len(rows), st.CellsShort)
		}
		h.Release()
		return pb.peak, st.CellsShort
	}
	short, cells := peak(1000, true)
	control, _ := peak(1000, false)
	t.Logf("peak %d bytes for %d short cells, %d for the same journal with valid cells", short, cells, control)
	if short > control+64<<10 {
		t.Errorf("short cells keep their charge: peak %d against %d for valid cells", short, control)
	}
}
