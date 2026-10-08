package records

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func FuzzTranslateRange(f *testing.F) {
	enc := func(vals ...int64) []byte {
		b := make([]byte, 0, 8*len(vals))
		for _, v := range vals {
			b = binary.LittleEndian.AppendUint64(b, uint64(v))
		}
		return b
	}
	for _, inc := range []bool{false, true} {
		f.Add(enc(1000, 100), int64(100), inc, int64(10), int64(20))
		f.Add(enc(1000, 50, 5000, 50), int64(100), inc, int64(40), int64(20))
		f.Add(enc(-1, 10, 3000, 10), int64(20), inc, int64(0), int64(20))
		f.Add(enc(0, 1000), int64(100), inc, int64(50), int64(50))
		f.Add([]byte{}, int64(0), inc, int64(0), int64(0))
	}
	f.Fuzz(func(t *testing.T, raw []byte, size int64, incomplete bool, off, length int64) {
		var runs []evidence.Run
		for len(raw) >= 16 && len(runs) < 2000 {
			runs = append(runs, evidence.Run{
				Offset: int64(binary.LittleEndian.Uint64(raw)),
				Length: int64(binary.LittleEndian.Uint64(raw[8:])),
			})
			raw = raw[16:]
		}
		r := Range{off, length}
		got, err := TranslateRange(runs, size, incomplete, r)
		again, err2 := TranslateRange(runs, size, incomplete, r)
		if !reflect.DeepEqual(got, again) || (err == nil) != (err2 == nil) {
			t.Fatal("not deterministic")
		}
		if err != nil {
			if !reflect.DeepEqual(got, Translation{}) {
				t.Fatal("partial result with error")
			}
			return
		}
		var sum, pos int64
		for i, e := range got.Extents {
			if i == 0 {
				pos = e.ArtifactOffset
			}
			if e.ArtifactOffset != pos || e.Length <= 0 {
				t.Fatalf("extent %d not contiguous: %+v", i, e)
			}
			pos += e.Length
			sum += e.Length
			if e.Hole != (e.ImageOffset < 0) {
				t.Fatal("hole flag")
			}
			if !e.Hole {
				inRun := false
				for _, ru := range runs {
					if ru.Offset >= 0 && e.ImageOffset >= ru.Offset && e.ImageOffset+e.Length <= ru.Offset+ru.Length {
						inRun = true
						break
					}
				}
				if !inRun {
					t.Fatalf("extent %+v lies in no run", e)
				}
			}
		}
		if !got.Truncated && sum != length {
			t.Fatalf("sum %d != %d", sum, length)
		}
		if len(got.Extents) > MaxExtentsPerHop || got.Total < len(got.Extents) {
			t.Fatal("caps")
		}
	})
}
