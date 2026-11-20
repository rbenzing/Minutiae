package tl

import (
	"runtime"
	"testing"
)

// Spec section 7.1 and section 12: a declared length or count never drives an allocation.
// A string declaring 8 MiB and a vector declaring MaxVector elements, each with 8 bytes left,
// read 100 times: the heap grows by less than 64 KiB in all.
func TestTLReaderNeverAllocatesFromDeclaredSize(t *testing.T) {
	str := append([]byte{0xfe, 0xff, 0xff, 0x7f}, make([]byte, 8)...)
	v := vec(MaxVector, 8)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 100 {
		r, err := NewReader(str)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.String(); err == nil {
			t.Fatal("String of 0x7fffff bytes with 8 left succeeded")
		}
		r, err = NewReader(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.VectorHeader(4); err == nil {
			t.Fatal("VectorHeader of MaxVector with 8 bytes left succeeded")
		}
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew >= 64<<10 {
		t.Errorf("TotalAlloc grew by %d bytes, want < 64 KiB", grew)
	}
	// A successful String allocates nothing: it returns a sub-slice.
	r := mustReader(t, []byte{3, 'a', 'b', 'c'})
	allocs := testing.AllocsPerRun(100, func() {
		r.off, r.steps = 0, 0
		if _, err := r.String(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("String allocated %v times per run, want 0", allocs)
	}
}
