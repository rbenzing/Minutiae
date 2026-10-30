package carve

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// TestWindowByteChargesWhatItReads: a one-byte look costs one chunk of the
// budget, not the whole 64 KiB cache, and a full sequential walk of the cache
// costs exactly the cache (whole-branch finding 3).
func TestWindowByteChargesWhatItReads(t *testing.T) {
	b := newBudget(1 << 30)
	w := newWindow(context.Background(), bytes.NewReader(make([]byte, 1<<20)), 0, 1<<20, b)
	if _, err := w.Byte(7); err != nil {
		t.Fatal(err)
	}
	if b.spent != windowChunk {
		t.Errorf("one look charged %d, want %d", b.spent, windowChunk)
	}
	for i := range int64(windowCacheSize) {
		if _, err := w.Byte(i); err != nil {
			t.Fatal(err)
		}
	}
	if b.spent != windowCacheSize {
		t.Errorf("a full walk charged %d, want %d", b.spent, windowCacheSize)
	}
}

// TestScanDensePlantedHeadersLeaveBudget: headers that fail on the first byte
// they test, planted on every 512-byte unit of the first half of a scope, must
// not spend the validate budget that a real object after them needs.
func TestScanDensePlantedHeadersLeaveBudget(t *testing.T) {
	const scope = 4 << 20
	img := carvetest.Image(scope, fillPattern)
	for off := 0; off < scope/2; off += 512 {
		carvetest.Place(img, off, []byte("ONEBYTEH"))
	}
	carvetest.Place(img, scope-4096, slices.Concat([]byte("ONEBYTEH"), []byte{'K'}, make([]byte, 32)))
	ob := toyType("onebyte", "ONEBYTEH", "test-onebyte")
	hits, st, err := runScan(t, bytes.NewReader(img), whole(img), toySet(ob), Options{Align: 512, ValidateBudget: 2 * scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != scope-4096 || st.BudgetExhausted {
		t.Errorf("hits %+v exhausted %v rejected %v, want the one real object", hits, st.BudgetExhausted, st.Rejected)
	}
}

// TestScanClampsLongBasisAndAssumptions: any entry a validator returns is cut
// to MaxEntryBytes plus a visible marker, whatever the validator (whole-branch
// finding 1).
func TestScanClampsLongBasisAndAssumptions(t *testing.T) {
	img := carvetest.Image(8192, fillPattern)
	carvetest.Place(img, 0, []byte("LONGTEXT"))
	lt := toyType("longtext", "LONGTEXT", "test-long")
	hits, _, err := runScan(t, bytes.NewReader(img), whole(img), toySet(lt), Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	for _, e := range append(slices.Clone(hits[0].Basis), hits[0].Assumptions...) {
		if len(e) > MaxEntryBytes+MaxEntryMarkerBytes {
			t.Errorf("entry of %d bytes not clamped", len(e))
		}
	}
	if !strings.HasSuffix(hits[0].Basis[0], "...(cut 400 bytes)") && !strings.Contains(hits[0].Basis[0], "...(cut ") {
		t.Errorf("basis %q has no visible cut marker", hits[0].Basis[0])
	}
	if !strings.Contains(hits[0].Assumptions[0], "...(cut ") {
		t.Errorf("assumption %q has no visible cut marker", hits[0].Assumptions[0])
	}
	if hits[0].Basis[1] != "short" {
		t.Errorf("short entry changed: %q", hits[0].Basis[1])
	}
}

// TestGzipHostileNameStaysShort: 64 bytes of 0xFF escape to 256 bytes; the
// basis is bounded after escaping, and the object is still a bounded hit.
func TestGzipHostileNameStaysShort(t *testing.T) {
	for _, name := range []string{strings.Repeat("\xff", 64), strings.Repeat("\x01", 64), strings.Repeat("é", 32)} {
		v := validateGzip(gzipWindow(carvetest.GzipHeader(0x08, name, ""), newBudget(1<<30)))
		if v.Outcome != Bounded || len(v.Basis) != 1 {
			t.Fatalf("verdict %+v", v)
		}
		if len(v.Basis[0]) > MaxEntryBytes {
			t.Errorf("basis is %d bytes, want at most %d", len(v.Basis[0]), MaxEntryBytes)
		}
	}
}
