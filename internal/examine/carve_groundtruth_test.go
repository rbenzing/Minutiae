package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
)

// Ground truth of carving (3E Task 14): the real fixtures (committed images whose oracle was read from the
// finished bytes by tools, never by Minutiae) are imported into fresh cases and carved with the real drivers.
// The match is exact: no percentage.

// gtImage is the oracle row of one image.
type gtImage struct {
	Name           string `json:"name"`
	ImageSHA       string `json:"image_sha256"`
	PartitionStart int64  `json:"partition_start"`
	Objects        []carveImageObject
	Controls       []gtControl `json:"controls"`
}

type gtControl struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ImageOffset int64  `json:"image_offset"`
	Length      int64  `json:"length"`
	SHA256      string `json:"sha256"`
}

// gtLoad returns the oracle row and the bytes of an image, the image hash checked first.
func gtLoad(t *testing.T, name string) (gtImage, []byte) {
	t.Helper()
	oracle := "carve.expect.json"
	if name == "carve-images" {
		oracle = "carve-images.expect.json" // 3E2: its own oracle
	}
	raw, err := os.ReadFile(filepath.Join(carveImagesDir, oracle))
	if err != nil {
		t.Fatalf("the image oracle is missing: %v", err)
	}
	var exp struct {
		Images []json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	for _, r := range exp.Images {
		var im gtImage
		if err := json.Unmarshal(r, &im); err != nil {
			t.Fatal(err)
		}
		if im.Name != name {
			continue
		}
		var objs struct {
			Objects []carveImageObject `json:"objects"`
		}
		if err := json.Unmarshal(r, &objs); err != nil {
			t.Fatal(err)
		}
		im.Objects = objs.Objects
		gz, err := os.ReadFile(filepath.Join(carveImagesDir, name+".img.gz"))
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		img, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if got := carveSum(img); got != im.ImageSHA {
			t.Fatalf("%s: image sha256 %s, oracle says %s: regenerate image and oracle together", name, got, im.ImageSHA)
		}
		return im, img
	}
	t.Fatalf("no oracle row for %s", name)
	return gtImage{}, nil
}

// gtEnv is a fresh case holding the image, with a session over it (the real filesystem drivers).
type gtEnv struct {
	c   *evidence.Case
	s   *examine.Session
	im  gtImage
	img []byte
}

func gtNew(t *testing.T, name string) *gtEnv {
	t.Helper()
	im, img := gtLoad(t, name)
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := filepath.Join(t.TempDir(), name+".img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := examine.Import(context.Background(), c, "img", []string{p}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &gtEnv{c: c, s: s, im: im, img: img}
}

func (e *gtEnv) carve(t *testing.T, o examine.CarveOptions) examine.CarveSummary {
	t.Helper()
	if o.Partition == 0 {
		o.Partition = -1
	}
	cs, err := e.s.Carve(context.Background(), o)
	if err != nil {
		t.Fatalf("Carve %+v: %v", o, err)
	}
	return cs
}

// gtCarved is one written carve artifact as the case records it.
type gtCarved struct {
	Rec    evidence.ManifestRecord
	Offset int64
	Length int64
	Type   string
	Method string
	Scope  string
	Alloc  evidence.AllocSummary
	Assum  []string
}

func (g gtCarved) key() string {
	return fmt.Sprintf("%d/%d/%s/%s", g.Offset, g.Length, g.Rec.SHA256, g.Type)
}

// gtArtifacts lists the carve artifacts of the case.
func gtArtifacts(t *testing.T, c *evidence.Case) []gtCarved {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var out []gtCarved
	for _, r := range recs {
		if r.Source.Kind != evidence.KindCarve {
			continue
		}
		d := r.Source.Derived
		if d == nil || d.Recovery == nil || len(d.Runs) != 1 {
			t.Fatalf("%s: a carve artifact needs exactly one run and a recovery: %+v", r.Path, d)
		}
		base := strings.TrimSuffix(filepath.Base(r.Path), filepath.Ext(r.Path))
		_, typ, ok := strings.Cut(base, "-")
		if !ok {
			t.Fatalf("%s: no type in the artifact name", r.Path)
		}
		if r.Incomplete {
			t.Errorf("%s: incomplete", r.Path)
		}
		out = append(out, gtCarved{
			Rec: r, Offset: d.Runs[0].Offset, Length: d.Runs[0].Length, Type: typ,
			Method: d.Recovery.Method, Scope: d.Recovery.Scope, Alloc: d.Recovery.Alloc, Assum: d.Recovery.Assumptions,
		})
	}
	return out
}

// gtWant is the sorted keys of the oracle objects with the given expectation that exist in scope.
func gtWant(im gtImage, scope, expect string) []string {
	var out []string
	for _, o := range im.Objects {
		if o.Expect != expect || (scope == examine.CarveUnallocated && o.State != "deleted") {
			continue
		}
		out = append(out, fmt.Sprintf("%d/%d/%s/%s", o.ImageOffset, o.Length, o.SHA256, o.Type))
	}
	slices.Sort(out)
	return out
}

func gtKeys(as []gtCarved, method string) []string {
	var out []string
	for _, a := range as {
		if a.Method == method {
			out = append(out, a.key())
		}
	}
	slices.Sort(out)
	return out
}

// gtScopeRuns are the image runs of the unallocated scope, read by the filesystem driver alone.
func gtScopeRuns(t *testing.T, e *gtEnv) [][2]int64 {
	t.Helper()
	start := e.im.PartitionStart
	r := io.NewSectionReader(bytes.NewReader(e.img), start, int64(len(e.img))-start)
	fsys, err := detect.Open(r, r.Size())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := fsys.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]int64
	for _, ru := range runs {
		out = append(out, [2]int64{start + ru.Offset, start + ru.Offset + ru.Length})
	}
	return out
}

type gtCase struct{ image, scope string }

var gtCases = []gtCase{
	{"carve-fat12", examine.CarveUnallocated},
	{"carve-fat12", examine.CarveRaw},
	{"carve-ext4", examine.CarveUnallocated},
	{"carve-ext4", examine.CarveRaw},
	{"carve-raw", examine.CarveRaw},
}

func gtAt(as []gtCarved, off int64) []gtCarved {
	var out []gtCarved
	for _, a := range as {
		if a.Offset == off {
			out = append(out, a)
		}
	}
	return out
}

func gtOverlaps(a gtCarved, off, n int64) bool { return a.Offset < off+n && off < a.Offset+a.Length }

func TestCarveMatchesGroundTruth(t *testing.T) {
	for _, gc := range gtCases {
		t.Run(gc.image+"/"+gc.scope, func(t *testing.T) {
			e := gtNew(t, gc.image)
			cs := e.carve(t, examine.CarveOptions{Scope: gc.scope, MinConfidence: examine.DefaultCarveMinConfidence})
			as := gtArtifacts(t, e.c)
			if cs.Carved != len(as) {
				t.Errorf("summary says %d carved, the case holds %d", cs.Carved, len(as))
			}
			for _, expect := range []string{"validated", "footer"} {
				if got, want := gtKeys(as, "carve-"+expect), gtWant(e.im, gc.scope, expect); !slices.Equal(got, want) {
					t.Errorf("carve-%s: artifacts differ from the oracle\n got  %q\n want %q", expect, got, want)
				}
			}
			live := 0
			for _, a := range as {
				if a.Method != "carve-validated" && a.Method != "carve-footer" {
					t.Errorf("%s: method %s at the default floor", a.Rec.Path, a.Method)
				}
				if a.Scope != gc.scope {
					t.Errorf("%s: scope %q, want %q", a.Rec.Path, a.Scope, gc.scope)
				}
				if got := carveSum(e.img[a.Offset : a.Offset+a.Length]); got != a.Rec.SHA256 {
					t.Errorf("%s: image bytes at %d hash %s, the artifact %s", a.Rec.Path, a.Offset, got, a.Rec.SHA256)
				}
				if a.Alloc.Allocated == a.Length {
					live++
				}
			}
			wantLive := 0
			for _, o := range e.im.Objects {
				if o.State == "live" && gc.scope == examine.CarveRaw {
					wantLive++
				}
			}
			if live != wantLive {
				t.Errorf("%d artifacts are fully allocated, the oracle has %d live objects in this scope", live, wantLive)
			}
			if gc.scope == examine.CarveUnallocated {
				runs := gtScopeRuns(t, e)
				for _, a := range as {
					if !slices.ContainsFunc(runs, func(r [2]int64) bool { return a.Offset >= r[0] && a.Offset+a.Length <= r[1] }) {
						t.Errorf("%s: run [%d,+%d) lies in no unallocated run %v", a.Rec.Path, a.Offset, a.Length, runs)
					}
					if a.Alloc.Free != a.Length {
						t.Errorf("%s: alloc %+v, want %d free", a.Rec.Path, a.Alloc, a.Length)
					}
				}
			}
		})
	}
}

func TestCarveNegativeControls(t *testing.T) {
	for _, gc := range gtCases {
		t.Run(gc.image+"/"+gc.scope, func(t *testing.T) {
			e := gtNew(t, gc.image)
			e.carve(t, examine.CarveOptions{Scope: gc.scope, MinConfidence: examine.DefaultCarveMinConfidence})
			as := gtArtifacts(t, e.c)
			for _, c := range e.im.Controls {
				switch c.Kind {
				case "random", "text", "truncated":
					for _, a := range as {
						if gtOverlaps(a, c.ImageOffset, c.Length) {
							t.Errorf("%s control at %d: artifact %s intersects it", c.Kind, c.ImageOffset, a.Rec.Path)
						}
					}
				case "fragmented":
					// Its whole-file hash equals that of an intact object elsewhere, so the hash may appear only
					// at that object; nothing at the first fragment may have the whole length.
					for _, a := range as {
						if a.Offset == c.ImageOffset && a.Length == c.Length {
							t.Errorf("fragmented control at %d carved whole: %s", c.ImageOffset, a.Rec.Path)
						}
						if a.Rec.SHA256 == c.SHA256 {
							intact := false
							for _, o := range e.im.Objects {
								intact = intact || (o.SHA256 == c.SHA256 && o.ImageOffset == a.Offset)
							}
							if !intact {
								t.Errorf("fragmented control: artifact %s at %d has its sha256 but is no intact object", a.Rec.Path, a.Offset)
							}
						}
					}
				default:
					t.Errorf("unknown control kind %q", c.Kind)
				}
			}
			for _, o := range e.im.Objects {
				switch o.Expect {
				case "not-in-set":
					for _, a := range as {
						if gtOverlaps(a, o.ImageOffset, o.Length) {
							t.Errorf("%s (not in the signature set) intersects artifact %s", o.Name, a.Rec.Path)
						}
					}
				case "footer":
					want := fmt.Sprintf("%d/%d/%s/%s", o.ImageOffset, o.Length, o.SHA256, o.Type)
					if o.Family == "pdf" && !slices.Contains(gtKeys(as, "carve-footer"), want) {
						t.Errorf("%s: no carve-footer artifact %s", o.Name, want)
					}
				case "unaligned", "nested", "bounded":
					if len(gtAt(as, o.ImageOffset)) != 0 {
						t.Errorf("%s (%s) is carved with the default options", o.Name, o.Expect)
					}
				}
			}
		})
	}
}

func gtObjectExpecting(t *testing.T, im gtImage, expect string) carveImageObject {
	t.Helper()
	for _, o := range im.Objects {
		if o.Expect == expect {
			return o
		}
	}
	t.Fatalf("%s has no %q object", im.Name, expect)
	return carveImageObject{}
}

func TestCarveUnalignedNestedAndBounded(t *testing.T) {
	t.Run("unaligned", func(t *testing.T) {
		for _, gc := range []gtCase{{"carve-fat12", examine.CarveUnallocated}, {"carve-raw", examine.CarveRaw}} {
			e := gtNew(t, gc.image)
			o := gtObjectExpecting(t, e.im, "unaligned")
			e.carve(t, examine.CarveOptions{Scope: gc.scope, Unaligned: true, MinConfidence: examine.DefaultCarveMinConfidence})
			found := gtAt(gtArtifacts(t, e.c), o.ImageOffset)
			if len(found) != 1 {
				t.Fatalf("%s: %d artifacts at the unaligned offset %d, want 1", gc.image, len(found), o.ImageOffset)
			}
			a := found[0]
			if a.Method != "carve-validated" || a.Length != o.Length || a.Rec.SHA256 != o.SHA256 || a.Type != o.Type {
				t.Errorf("%s: unaligned carve %s, want validated %d bytes %s %s", gc.image, a.key(), o.Length, o.SHA256, o.Type)
			}
		}
	})
	t.Run("nested", func(t *testing.T) {
		e := gtNew(t, "carve-fat12")
		o := gtObjectExpecting(t, e.im, "nested")
		e.carve(t, examine.CarveOptions{Scope: examine.CarveUnallocated, Nested: true, MinConfidence: examine.DefaultCarveMinConfidence})
		found := gtAt(gtArtifacts(t, e.c), o.ImageOffset)
		if len(found) != 1 {
			t.Fatalf("%d artifacts at the nested offset %d, want 1", len(found), o.ImageOffset)
		}
		a := found[0]
		want := fmt.Sprintf("nested-in=%016x", *o.NestedIn)
		if a.Length != o.Length || a.Rec.SHA256 != o.SHA256 || a.Type != o.Type || !slices.Contains(a.Assum, want) {
			t.Errorf("nested carve %s (assumptions %q), want %d bytes %s %s with %s", a.key(), a.Assum, o.Length, o.SHA256, o.Type, want)
		}
	})
	t.Run("gzip", func(t *testing.T) {
		for _, gc := range gtCases {
			e := gtNew(t, gc.image)
			var gz []carveImageObject
			for _, o := range e.im.Objects {
				if o.Expect == "bounded" && (gc.scope != examine.CarveUnallocated || o.State == "deleted") {
					gz = append(gz, o)
				}
			}
			if len(gz) == 0 {
				continue
			}
			cs := e.carve(t, examine.CarveOptions{Scope: gc.scope, MinConfidence: examine.DefaultCarveMinConfidence})
			if cs.SkippedBy["below-min-confidence"] != len(gz) {
				t.Errorf("%s/%s: below-min-confidence counted %d, want %d", gc.image, gc.scope, cs.SkippedBy["below-min-confidence"], len(gz))
			}
			e = gtNew(t, gc.image)
			e.carve(t, examine.CarveOptions{Scope: gc.scope, MinConfidence: 20})
			as := gtArtifacts(t, e.c)
			for _, o := range gz {
				found := gtAt(as, o.ImageOffset)
				if len(found) != 1 || found[0].Method != "carve-bounded" {
					t.Errorf("%s/%s: want one carve-bounded artifact at %d, got %d", gc.image, gc.scope, o.ImageOffset, len(found))
					continue
				}
				b, err := os.ReadFile(filepath.Join(e.c.Dir, filepath.FromSlash(found[0].Rec.Path)))
				if err != nil {
					t.Fatal(err)
				}
				if int64(len(b)) < o.Length || carveSum(b[:o.Length]) != o.SHA256 {
					t.Errorf("%s/%s: the first %d bytes of the bounded artifact (%d bytes) do not hash to %s", gc.image, gc.scope, o.Length, len(b), o.SHA256)
				}
			}
		}
	})
}

func TestCarveFixtureVerifyClean(t *testing.T) {
	for _, name := range []string{"carve-fat12", "carve-ext4", "carve-raw"} {
		t.Run(name, func(t *testing.T) {
			e := gtNew(t, name)
			scopes := []string{examine.CarveUnallocated, examine.CarveRaw}
			if name == "carve-raw" {
				scopes = scopes[1:]
			}
			for _, sc := range scopes {
				e.carve(t, examine.CarveOptions{Scope: sc, MinConfidence: examine.DefaultCarveMinConfidence})
			}
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
			if rep.AllocationRechecked != carved {
				t.Errorf("AllocationRechecked = %d, want the %d carved", rep.AllocationRechecked, carved)
			}
		})
	}
}
