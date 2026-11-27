package tl

import (
	"bytes"
	"slices"
	"testing"
)

// FuzzTLDecodeSynthetic (plan 4I4 Task 3, 4I4-P13; plan 4I4c W1-W3 adds Pair, Mixed and
// Bare; the Telegram table's FuzzTLDecode lives in the parser package): Decode over random
// bytes with the synthetic table and a counting budget never panics; End <= len(b); a decode
// without a stop ends at the end of the top object (decoding b[:End] gives the same result);
// every path is unique; no KSecret field holds bytes; the budget holds exactly 128 bytes and
// the path's length per field (F8, I1).
func FuzzTLDecodeSynthetic(f *testing.F) {
	f.Add(fullBox(), uint8(0))
	f.Add(boxBlob(nil, nil, nil, nil, 0), uint8(0))
	f.Add(boxBlob(le32(idBlank), nil, nil, nil, 0), uint8(0))
	f.Add(boxBlob(nil, [][]byte{cat(le32(idHalf), le32(1<<5), le32(1))}, nil, nil, 0), uint8(0))
	f.Add(pointBlob(3, 1, 2), uint8(1))
	f.Add(cat(le32(idNode), le32(idNode), le32(idNode)), uint8(2))
	f.Add(cat(le32(idKinds), le32(IDBoolTrue), le64(0), tlBytes([]byte("hi")), tlBytes(nil), le32(IDVector), le32(1), le32(7), le64(9)), uint8(3))
	f.Add(crateBlob(pointsBox(2), 2, cat(le32(idNode), le32(idNode))), uint8(4))
	// Plan 4I4c W1-W3: a pair of one ID in two types, a union vector (each type, an opaque first
	// row, an ID in neither) and bare id and string vectors.
	f.Add(cat(le32(idPair), le32(idTwin), le32(7), le32(idTwin), le64(1<<40)), uint8(5))
	f.Add(mixedBlob(cat(le32(idLeft), le32(5)), cat(le32(idRight), le64(6)), cat(le32(idTwin), le32(3))), uint8(6))
	f.Add(mixedBlob(cat(le32(idShared), le32(9)), cat(le32(0xdeadbeef), le32(0))), uint8(6))
	f.Add(bareBlob([]int32{7, -1}, []int64{1 << 40}, [][]byte{nil, []byte("hi"), bytes.Repeat([]byte{'b'}, 254)}), uint8(7))
	types := []string{"Box", "Point", "Node", "Kinds", "Crate", "Pair", "Mixed", "Bare"}
	tab := synthTable()
	f.Fuzz(func(t *testing.T, b []byte, which uint8) {
		typ := types[int(which)%len(types)]
		bud := open()
		r := Decode(b, typ, tab, bud)
		if r.End < 0 || r.End > len(b) {
			t.Fatalf("end %d of %d", r.End, len(b))
		}
		if bud.held != wantCharge(r) || r.Charged() != bud.held {
			t.Fatalf("budget holds %d, Charged %d; want %d", bud.held, r.Charged(), wantCharge(r))
		}
		seen := make(map[string]bool, len(r.Fields))
		for _, n := range r.Fields {
			if seen[n.Path] {
				t.Fatalf("path %q twice", n.Path)
			}
			seen[n.Path] = true
			if n.Kind == KSecret && n.Bytes != nil {
				t.Fatalf("%s: a secret holds bytes", n.Path)
			}
		}
		if r.Stop != nil {
			return
		}
		again := Decode(b[:r.End], typ, tab, open())
		if again.Stop != nil || again.End != r.End || !slices.EqualFunc(again.Fields, r.Fields, sameNamed) {
			t.Fatalf("b[:End] decodes differently: stop %v, end %d of %d, %d of %d fields",
				again.Stop, again.End, r.End, len(again.Fields), len(r.Fields))
		}
	})
}
