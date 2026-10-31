package examine_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/examine"
)

// Ground truth of carving the image formats (3E2 Task 8): carve-images holds
// the real-encoder objects of images.expect.json at known offsets, plus text,
// truncated and random controls. Its oracle was written by the generator from
// the encoders' own listing tools, never by Minutiae. gtNew checks image_sha256
// first ("regenerate image and oracle together").

const imagesFixture = "carve-images"

// imagesOverreach names the truncated controls that validate at the declared length of
// the object they were cut from (see TestCarveImagesNegativeControls), by original object.
var imagesOverreach = map[string]string{"bmp-v3-24.bmp.truncated": "bmp-v3-24.bmp", "ico-dib.ico.truncated": "ico-dib.ico"}

func TestCarveImagesMatchesGroundTruth(t *testing.T) {
	e := gtNew(t, imagesFixture)
	cs := e.carve(t, examine.CarveOptions{Scope: examine.CarveRaw, MinConfidence: examine.DefaultCarveMinConfidence})
	as := gtArtifacts(t, e.c)
	if cs.Carved != len(as) {
		t.Errorf("summary says %d carved, the case holds %d", cs.Carved, len(as))
	}
	// Measured: tiff-le-pages.tif (Pillow) ends with 4 zero bytes after the last strip
	// that no offset references. The TIFF carve is the structure's maximum end, so for a
	// TIFF the expected object is the oracle's bytes up to the artifact's length, and the
	// rest must be all zero and under 8 bytes (encoder word padding); anything else fails.
	want := gtWant(e.im, examine.CarveRaw, "validated")
	for i, o := range e.im.Objects {
		if o.Family != "tiff" {
			continue
		}
		for _, a := range gtAt(as, o.ImageOffset) {
			if a.Length >= o.Length {
				continue
			}
			rest := e.img[o.ImageOffset+a.Length : o.ImageOffset+o.Length]
			if len(rest) >= 8 || slices.ContainsFunc(rest, func(b byte) bool { return b != 0 }) {
				t.Errorf("%s: carved %d of %d bytes, the rest %x is not short zero padding", o.Name, a.Length, o.Length, rest)
				continue
			}
			old := imagesKey(o)
			o.Length = a.Length
			o.SHA256 = carveSum(e.img[o.ImageOffset : o.ImageOffset+a.Length])
			e.im.Objects[i] = o
			j := slices.Index(want, old)
			if j < 0 {
				t.Fatalf("%s: oracle key %s not in the expected set", o.Name, old)
			}
			want[j] = imagesKey(o)
		}
	}
	// The two declared-length over-reaches are carve-validated too: the declared length of
	// the original object, at the control's offset, over the bytes found there.
	nOver := 0
	for _, c := range e.im.Controls {
		orig, ok := imagesOverreach[c.Name]
		if !ok {
			continue
		}
		for _, o := range e.im.Objects {
			if o.Name == orig {
				nOver++
				o.ImageOffset = c.ImageOffset
				o.SHA256 = carveSum(e.img[o.ImageOffset : o.ImageOffset+o.Length])
				want = append(want, imagesKey(o))
			}
		}
	}
	slices.Sort(want)
	if got := gtKeys(as, "carve-validated"); !slices.Equal(got, want) {
		t.Errorf("carve-validated artifacts differ from the oracle\n got  %q\n want %q", got, want)
	}
	if len(as) != len(e.im.Objects)+nOver {
		t.Errorf("%d artifacts, the oracle has %d objects and %d declared-length over-reaches", len(as), len(e.im.Objects), nOver)
	}
	for _, a := range as {
		if a.Method != "carve-validated" {
			t.Errorf("%s: method %s at the default floor (no carve-bounded or carve-footer is expected)", a.Rec.Path, a.Method)
		}
		if a.Scope != examine.CarveRaw {
			t.Errorf("%s: scope %q", a.Rec.Path, a.Scope)
		}
		if got := carveSum(e.img[a.Offset : a.Offset+a.Length]); got != a.Rec.SHA256 {
			t.Errorf("%s: image bytes at %d hash %s, the artifact %s", a.Rec.Path, a.Offset, got, a.Rec.SHA256)
		}
		// The raw scope is one run covering the whole image.
		if a.Offset < 0 || a.Offset+a.Length > int64(len(e.img)) {
			t.Errorf("%s: run [%d,+%d) leaves the image", a.Rec.Path, a.Offset, a.Length)
		}
	}
	if len(cs.Stats.BoundedBy) != 0 {
		t.Errorf("bounded verdicts counted on a fixture of complete objects: %v", cs.Stats.BoundedBy)
	}
}

func TestCarveImagesNegativeControls(t *testing.T) {
	e := gtNew(t, imagesFixture)
	cs := e.carve(t, examine.CarveOptions{Scope: examine.CarveRaw, MinConfidence: examine.DefaultCarveMinConfidence})
	as := gtArtifacts(t, e.c)
	if len(e.im.Controls) == 0 {
		t.Fatal("the oracle lists no control")
	}
	// Declared-length over-reach (a limitation, docs/limitations.md): BMP and ICO have no
	// checksum, so a truncated one followed by other bytes of the window validates at its
	// declared length. Scanned on a window that ends at the cut, it is rejected.
	overreach := imagesOverreach
	var over []gtCarved
	for _, c := range e.im.Controls {
		orig, ok := overreach[c.Name]
		if !ok {
			continue
		}
		var o *carveImageObject
		for i := range e.im.Objects {
			if e.im.Objects[i].Name == orig {
				o = &e.im.Objects[i]
			}
		}
		if o == nil {
			t.Fatalf("no object %s for the control %s", orig, c.Name)
		}
		found := gtAt(as, c.ImageOffset)
		if len(found) != 1 || found[0].Method != "carve-validated" || found[0].Length != o.Length || found[0].Type != o.Type {
			t.Errorf("%s: want one validated %s carve of the declared %d bytes at %d, got %+v", c.Name, o.Type, o.Length, c.ImageOffset, found)
			continue
		}
		if found[0].Rec.SHA256 == o.SHA256 {
			t.Errorf("%s: the over-reaching carve hashes like the intact object", c.Name)
		}
		over = append(over, found[0])
	}
	if len(over) != len(overreach) {
		t.Errorf("%d over-reach controls found, want %d", len(over), len(overreach))
	}
	kinds := map[string]int{}
	for _, c := range e.im.Controls {
		kinds[c.Kind]++
		_, isOver := overreach[c.Name]
		for _, a := range as {
			if isOver && a.Offset == c.ImageOffset {
				continue
			}
			if slices.ContainsFunc(over, func(o gtCarved) bool { return o.Offset == a.Offset }) && a.Offset != c.ImageOffset {
				continue // the over-reaching carve covers the controls that follow it
			}
			if gtOverlaps(a, c.ImageOffset, c.Length) {
				t.Errorf("%s control %s at %d: artifact %s intersects it", c.Kind, c.Name, c.ImageOffset, a.Rec.Path)
			}
		}
		// Scanned alone, a truncated or random control opens a window at its header
		// and the validator rejects it, counted by reason; none is accepted.
		var accepted int
		st, err := carve.Scan(context.Background(), bytes.NewReader(e.img), []carve.Run{{Offset: c.ImageOffset, Length: c.Length}},
			carve.Builtin(), carve.Options{Align: 512}, func(carve.Hit) error { accepted++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		rejected := 0
		for _, n := range st.Rejected {
			rejected += n
		}
		if accepted != 0 {
			t.Errorf("%s control at %d: %d hits accepted", c.Kind, c.ImageOffset, accepted)
		}
		if (c.Kind == "truncated" || c.Kind == "random") && rejected == 0 {
			t.Errorf("%s control at %d: no rejection counted (Stats.Rejected %v)", c.Kind, c.ImageOffset, st.Rejected)
		}
	}
	for _, k := range []string{"text", "truncated", "random"} {
		if kinds[k] == 0 {
			t.Errorf("no %s control in the oracle", k)
		}
	}
	total := 0
	for _, n := range cs.Stats.Rejected {
		total += n
	}
	if total == 0 {
		t.Errorf("the carve counted no rejection (%v)", cs.Stats.Rejected)
	}
}

func TestCarveImagesVerifyClean(t *testing.T) {
	e := gtNew(t, imagesFixture)
	e.carve(t, examine.CarveOptions{Scope: examine.CarveRaw, MinConfidence: examine.DefaultCarveMinConfidence})
	carved := len(gtArtifacts(t, e.c))
	if carved == 0 {
		t.Fatal("nothing carved")
	}
	rep, err := e.c.VerifyWith(context.Background(), examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("verify problems: %q", rep.Problems)
	}
	if rep.RecoveredReproduced != carved {
		t.Errorf("RecoveredReproduced = %d, want the %d carved", rep.RecoveredReproduced, carved)
	}
}

func imagesKey(o carveImageObject) string {
	return fmt.Sprintf("%d/%d/%s/%s", o.ImageOffset, o.Length, o.SHA256, o.Type)
}
