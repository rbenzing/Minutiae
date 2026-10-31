package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestNewFormatsRealEncoderObjects: every object of images.expect.json (made by
// real encoders; the facts come from the encoders' own listing tools, never
// from these validators) is validated whole by its family's validator on a
// window of exactly the object, with the recorded size, type and counts.
func TestNewFormatsRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "images.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family, Type, Expect, Brand string
			Size                              int64
			Width, Height                     int
			Facts                             map[string]any
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, ty := range Builtin().Types {
		ids[ty.Name] = ty.Validate
	}
	validators := builtinValidators()
	byFamily := map[string]int{}
	for _, o := range exp.Objects {
		byFamily[o.Family]++
		t.Run(o.File, func(t *testing.T) {
			id, ok := ids[o.Family]
			if !ok {
				t.Fatalf("no built-in type named %q", o.Family)
			}
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(obj)) != o.Size || o.Expect != "validated" {
				t.Fatalf("fixture: %d bytes, expect %q; oracle size %d", len(obj), o.Expect, o.Size)
			}
			w := newWindow(context.Background(), bytes.NewReader(obj), 0, int64(len(obj)), newBudget(1<<30))
			v := validators[id](w)
			// A verdict that names no refinement leaves the type to the matched row.
			gotType := v.Type
			if gotType == "" {
				gotType = o.Family
			}
			// Measured: a Pillow multi-page TIFF (tiff-le-pages.tif) ends with 4 zero
			// bytes after the last strip that no offset references. The carve is the
			// structure's maximum end; only encoder word padding (all zero, under 8
			// bytes) may be left over, anything else stays a failure.
			wantLen := o.Size
			if o.Family == "tiff" && v.Outcome == Validated && v.Length < o.Size {
				if rest := obj[v.Length:]; len(rest) < 8 && !slices.ContainsFunc(rest, func(b byte) bool { return b != 0 }) {
					wantLen = v.Length
				}
			}
			if v.Outcome != Validated || v.Length != wantLen || gotType != o.Type {
				t.Fatalf("verdict = %+v, want validated length %d type %q", v, wantLen, o.Type)
			}
			if w.outsideReads() != 0 {
				t.Errorf("%d reads outside the window", w.outsideReads())
			}
			basis := func(prefix string) (string, bool) {
				for _, b := range v.Basis {
					if s, ok := strings.CutPrefix(b, prefix); ok {
						return s, true
					}
				}
				return "", false
			}
			if o.Width != 0 && o.Family != "ico" && o.Family != "cur" {
				got, ok := basis("dims=")
				if want := strconv.Itoa(o.Width) + "x" + strconv.Itoa(o.Height); ok && got != want {
					t.Errorf("dims = %s, want %s (basis %v)", got, want, v.Basis)
				} else if !ok && o.Family == "bmp" {
					t.Errorf("no dims= in basis %v", v.Basis)
				}
			}
			switch o.Family {
			case "tiff":
				// The listing tool counts the chained directories; the validator
				// counts every IFD it walks, so the Exif and GPS sub-IFDs the
				// oracle lists separately are added.
				subs, _ := o.Facts["sub_directories"].([]any)
				want := strconv.Itoa(int(o.Facts["directories"].(float64)) + len(subs))
				if got, _ := basis("ifds="); got != want {
					t.Errorf("ifds = %q, want %s (basis %v)", got, want, v.Basis)
				}
			case "ico", "cur":
				want := strconv.Itoa(int(o.Facts["entry_count"].(float64)))
				if got, _ := basis("images="); got != want {
					t.Errorf("images = %q, want %s (basis %v)", got, want, v.Basis)
				}
			case "isobmff":
				if got, _ := basis("brand="); got != o.Brand {
					t.Errorf("brand = %q, want %q (basis %v)", got, o.Brand, v.Basis)
				}
			}
		})
	}
	for _, fam := range []string{"bmp", "tiff", "ico", "cur", "isobmff"} {
		if byFamily[fam] == 0 {
			t.Errorf("no %s object in images.expect.json", fam)
		}
	}
	if !slices.Contains([]int{18}, len(exp.Objects)) {
		t.Errorf("%d objects, want 18", len(exp.Objects))
	}
}
