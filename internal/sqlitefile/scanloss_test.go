package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestScanTreeLossCountsWhatTheScanDropped: the per-scan loss counter is a
// real count of the pages and cells a scan did not deliver, not a warnings
// length. Warnings are de-duplicated; the counter is not, so a second scan of
// the same damage reports the same loss (B76).
func TestScanTreeLossCountsWhatTheScanDropped(t *testing.T) {
	scan := func(v *sqlitefile.View, root uint32) (int, sqlitefile.ScanLoss) {
		n := 0
		loss, err := v.ScanTreeLoss(context.Background(), root, sqlitefile.TableTree, func(sqlitefile.Row) bool { n++; return true })
		if err != nil {
			t.Fatalf("ScanTreeLoss: %v", err)
		}
		return n, loss
	}
	t.Run("a clean tree loses nothing", func(t *testing.T) {
		f := newTreeFixture(t, 300, false)
		_, v := openLive(t, f.b.Bytes(), sqlitefile.Options{})
		n, loss := scan(v, f.tb.Root())
		if n != 300 || loss.Any() {
			t.Errorf("rows %d, loss %+v", n, loss)
		}
	})
	t.Run("an emptied leaf is a lost page, on every scan", func(t *testing.T) {
		f := newTreeFixture(t, 300, false)
		data := f.b.Bytes()
		binary.BigEndian.PutUint16(pageAt(data, f.ps, f.tb.Leaves()[1])[3:], 0)
		_, v := openLive(t, data, sqlitefile.Options{})
		for i := range 3 {
			n, loss := scan(v, f.tb.Root())
			if n >= 300 || loss.Pages < 1 {
				t.Fatalf("scan %d: rows %d, loss %+v", i, n, loss)
			}
		}
	})
	t.Run("a pointer past the page is a lost cell or page", func(t *testing.T) {
		f := newTreeFixture(t, 300, false)
		data := f.b.Bytes()
		pg := pageAt(data, f.ps, f.tb.Leaves()[1])
		pg[8], pg[9] = 0xff, 0xff
		_, v := openLive(t, data, sqlitefile.Options{})
		n, loss := scan(v, f.tb.Root())
		if n >= 300 || !loss.Any() {
			t.Errorf("rows %d, loss %+v", n, loss)
		}
	})
	t.Run("an out of range child is a lost page", func(t *testing.T) {
		f := newTreeFixture(t, 300, false)
		data := f.b.Bytes()
		setChild(data, f.ps, f.tb.Root(), 0, 9999)
		_, v := openLive(t, data, sqlitefile.Options{})
		n, loss := scan(v, f.tb.Root())
		if n >= 300 || loss.Pages < 1 {
			t.Errorf("rows %d, loss %+v", n, loss)
		}
	})
}
