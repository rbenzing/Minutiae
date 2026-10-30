package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func init() { implementedValidators["pdf"] = true }

func pdfWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

func pdfVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validatePDF(pdfWindow(buf, newBudget(1<<30)))
}

func TestPDFFooterVariants(t *testing.T) {
	for name, c := range map[string]struct {
		o    carvetest.PDFOpts
		want string
	}{
		"one revision":   {carvetest.PDFOpts{}, "revisions=1"},
		"two increments": {carvetest.PDFOpts{Increments: 2}, "revisions=3"},
		"version 2.0":    {carvetest.PDFOpts{Version: "2.0"}, "revisions=1"},
		"version 1.0":    {carvetest.PDFOpts{Version: "1.0"}, "revisions=1"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.PDF(c.o)
			v := pdfVerdict(obj, 4096)
			if v.Outcome != Footer || v.Length != int64(len(obj)) || !slices.Contains(v.Basis, c.want) ||
				!slices.Contains(v.Assumptions, "last-eof-in-window") {
				t.Fatalf("verdict = %+v, want footer length %d with %s", v, len(obj), c.want)
			}
		})
	}
	// Line-ending variants around the number and after %%EOF.
	for name, c := range map[string]struct {
		text string
		drop int
	}{
		"CRLF":      {"startxref\r\nXREF\r\n%%EOF\r\n", 0},
		"CR":        {"startxref\rXREF\r%%EOF\r", 0},
		"LF":        {"startxref\nXREF\n%%EOF\n", 0},
		"no EOL":    {"startxref\nXREF\n%%EOF", 0},
		"EOL stops": {"startxref\nXREF\n%%EOF\nzzz", 3},
	} {
		t.Run(name, func(t *testing.T) {
			head := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n")
			text := bytes.Replace([]byte(c.text), []byte("XREF"), []byte(strconv.Itoa(len(head))), 1)
			obj := slices.Concat(head, []byte("xref\n0 1\n"), text)
			v := validatePDF(pdfWindow(obj, newBudget(1<<30)))
			if want := int64(len(obj) - c.drop); v.Outcome != Footer || v.Length != want {
				t.Fatalf("verdict = %+v, want footer length %d", v, want)
			}
		})
	}
	// An obj-style startxref target (a cross-reference stream).
	obj := []byte("%PDF-1.5\n12 0 obj\n<<>>\nendobj\nstartxref\n9\n%%EOF\n")
	if v := validatePDF(pdfWindow(obj, newBudget(1<<30))); v.Outcome != Footer || v.Length != int64(len(obj)) {
		t.Fatalf("xref stream verdict = %+v", v)
	}
	// An aligned %PDF- header ends the search: the second file's revision is
	// not part of the first.
	one := carvetest.PDF(carvetest.PDFOpts{})
	two := carvetest.PDF(carvetest.PDFOpts{Increments: 1})
	buf := slices.Concat(one, bytes.Repeat([]byte{0}, 512-len(one)%512), two)
	if v := validatePDF(pdfWindow(buf, newBudget(1<<30))); v.Outcome != Footer || v.Length != int64(len(one)) || !slices.Contains(v.Basis, "revisions=1") {
		t.Fatalf("aligned header verdict = %+v, want length %d", v, len(one))
	}
	// Not aligned: the search goes on and the last counting EOF ends the object
	// (the second file's offsets are relative to its own header, so only some of
	// its revisions count).
	buf = slices.Concat(one, bytes.Repeat([]byte{0}, 511-len(one)%512), two)
	if v := validatePDF(pdfWindow(buf, newBudget(1<<30))); v.Outcome != Footer || v.Length <= int64(len(one)) || v.Length > int64(len(buf)) {
		t.Fatalf("unaligned header verdict = %+v, want a length in (%d, %d]", v, len(one), len(buf))
	}
}

func TestPDFRejects(t *testing.T) {
	good := carvetest.PDF(carvetest.PDFOpts{})
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"version 3.0", carvetest.PDF(carvetest.PDFOpts{Version: "3.0"}), "bad-version"},
		{"version 1.x", carvetest.PDF(carvetest.PDFOpts{Version: "1.x"}), "bad-version"},
		{"version 0.9", carvetest.PDF(carvetest.PDFOpts{Version: "0.9"}), "bad-version"},
		{"version 1,7", carvetest.PDF(carvetest.PDFOpts{Version: "1,7"}), "bad-version"},
		{"bad startxref", carvetest.PDF(carvetest.PDFOpts{BadStartxref: true}), "no-eof"},
		{"bad startxref with increments", carvetest.PDF(carvetest.PDFOpts{BadStartxref: true, Increments: 2}), "no-eof"},
		{"no EOF", carvetest.PDF(carvetest.PDFOpts{NoEOF: true, Increments: 1}), "no-eof"},
		{"header only", []byte("%PDF-1.7\n"), "no-eof"},
		{"EOF without startxref", []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF\n"), "no-eof"},
		{"startxref without a number", []byte("%PDF-1.7\nstartxref\n%%EOF\n"), "no-eof"},
		{"startxref number past itself", []byte("%PDF-1.7\nxref\nstartxref\n999\n%%EOF\n"), "no-eof"},
		{"startxref number is the startxref", []byte("%PDF-1.7\nxref\nstartxref\n14\n%%EOF\n"), "no-eof"},
		{"startxref number over 15 digits", []byte("%PDF-1.7\nxref\nstartxref\n1234567890123456\n%%EOF\n"), "no-eof"},
		{"startxref number of 30 digits (wraps)", []byte("%PDF-1.7\nxref\nstartxref\n999999999999999999999999999999\n%%EOF\n"), "no-eof"},
		{"startxref number 20 digits wrapping to 9", []byte("%PDF-1.7\nxref\nstartxref\n18446744073709551625\n%%EOF\n"), "no-eof"},
		{"startxref number points after itself", []byte("%PDF-1.7\nstartxref\n28\n%%EOF\nxref\n"), "no-eof"},
		{"target is not a section", []byte("%PDF-1.7\n1 0 obj\nstartxref\n10\n%%EOF\n"), "no-eof"},
		{"target obj without numbers", []byte("%PDF-1.7\nobj \nstartxref\n9\n%%EOF\n"), "no-eof"},
		{"EOF cut short", good[:len(good)-3], "no-eof"},
		{"cut before startxref", good[:len(good)-20], "no-eof"},
		{"short header", []byte("%PDF-1"), "truncated"},
		{"bad signature", slices.Concat([]byte("%PDX-"), good[5:]), "bad-signature"},
		{"empty", nil, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validatePDF(pdfWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := pdfVerdict(good, 0); v.Outcome != Footer {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestPDFWindowBoundsAndBudget: a long search is bounded by the budget and by
// cancellation (rejected "unreadable", ruling 3E-C1) and finds nothing in
// hostile repeats of its own keywords.
func TestPDFWindowBoundsAndBudget(t *testing.T) {
	good := carvetest.PDF(carvetest.PDFOpts{})
	hostile := slices.Concat([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("startxref\n1\n%%EOF"), 1<<16))
	if v := validatePDF(pdfWindow(hostile, newBudget(1<<30))); v.Outcome != Rejected || v.Reason != "no-eof" {
		t.Fatalf("hostile verdict = %+v", v)
	}
	b := newBudget(128 << 10)
	v := validatePDF(pdfWindow(hostile, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(good), 0, int64(len(good)), newBudget(1<<30))
	if v := validatePDF(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

// TestPDFReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// header, in the search and at a startxref target is rejected "unreadable".
func TestPDFReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.PDF(carvetest.PDFOpts{Increments: 1})
	big := slices.Concat(obj[:20], bytes.Repeat([]byte{'x'}, 200000), obj[20:])
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at the header", obj, 0},
		{"mid search", big, 70000},
		{"early in the first block", obj, 25},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validatePDF(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
				t.Fatalf("verdict = %+v readErr = %v, want rejected unreadable", v, w.readErr)
			}
		})
	}
}

func TestPDFRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family, Expect string
			Size                 int64
			Facts                struct {
				Startxref  []int
				EOFOffsets []int `json:"eof_offsets"`
			}
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "pdf" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := pdfVerdict(obj, 4096)
			want := "revisions=" + strconv.Itoa(len(o.Facts.EOFOffsets))
			if o.Expect != "footer" || v.Outcome != Footer || v.Length != o.Size || int64(len(obj)) != o.Size ||
				len(o.Facts.EOFOffsets) == 0 || len(o.Facts.EOFOffsets) != len(o.Facts.Startxref) || !slices.Contains(v.Basis, want) {
				t.Fatalf("verdict = %+v, want footer length %d with %s", v, o.Size, want)
			}
		})
	}
	if n != 2 {
		t.Fatalf("%d pdf objects in the expectations, want 2", n)
	}
}

// TestPDFZeroPaddedStartxref: leading zeros do not count toward the 15-digit
// cap of the startxref offset.
func TestPDFZeroPaddedStartxref(t *testing.T) {
	obj := []byte("%PDF-1.7\nxref\nstartxref\n0000000000000000009\n%%EOF\n")
	if v := validatePDF(pdfWindow(obj, newBudget(1<<30))); v.Outcome != Footer || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want footer length %d", v, len(obj))
	}
}
