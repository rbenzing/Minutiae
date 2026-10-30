package carve

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// TestNewFormatsExactlyAtRunEnd: an object that ends exactly at its run end is
// validated at the boundary (a BMP bfSize, a TIFF's last strip, an ICO's last
// entry, a JPEG XL container's last box), and with the run one byte shorter the
// same bytes are a counted truncated rejection, never a hit.
func TestNewFormatsExactlyAtRunEnd(t *testing.T) {
	const at = 4096
	ico := carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{carvetest.PNG(carvetest.PNGOpts{}), carvetest.DIB(carvetest.BMPOpts{DIBSize: 40, W: 16, H: 32, BPP: 32, Planes: 1})}})
	for _, c := range []struct {
		name, family string
		obj          []byte
	}{
		{"bmp", "bmp", carvetest.BMP(carvetest.BMPOpts{DIBSize: 40, W: 64, H: 48, BPP: 24, Planes: 1})},
		{"tiff", "tiff", carvetest.TIFF(tiffOpts(tiffPage(false), tiffPage(false)))},
		{"bigtiff", "tiff", carvetest.TIFF(func() carvetest.TIFFOpts { o := tiffOpts(tiffPage(false)); o.Big = true; return o }())},
		{"ico", "ico", ico},
		{"jxl jxlc", "isobmff", carvetest.JXLContainer(carvetest.JXLOpts{})},
		{"jxl jxlp", "isobmff", carvetest.JXLContainer(carvetest.JXLOpts{Parts: 3})},
	} {
		t.Run(c.name, func(t *testing.T) {
			img := carvetest.Image(at+len(c.obj), fillPattern)
			carvetest.Place(img, at, c.obj)
			scan := func(runLen int) ([]Hit, Stats) {
				var hits []Hit
				st, err := Scan(context.Background(), bytes.NewReader(img), []Run{{0, int64(runLen)}}, Builtin(), Options{Align: 4096},
					func(h Hit) error { hits = append(hits, h); return nil })
				if err != nil {
					t.Fatal(err)
				}
				return hits, st
			}
			hits, _ := scan(len(img))
			if len(hits) != 1 || hits[0].Outcome != Validated || hits[0].Offset != at || hits[0].Length != int64(len(c.obj)) || hits[0].Family != c.family {
				t.Fatalf("at the run end: hits = %+v, want one validated %s hit at %d of length %d", hits, c.family, at, len(c.obj))
			}
			hits, st := scan(len(img) - 1)
			if len(hits) != 0 || st.Rejected["truncated"] != 1 {
				t.Fatalf("one byte short: hits = %+v, rejected = %v, want no hit and truncated once", hits, st.Rejected)
			}
		})
	}
}
