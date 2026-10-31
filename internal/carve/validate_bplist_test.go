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

func bplistWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

func bplistVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateBPlist(bplistWindow(buf, newBudget(1<<30)))
}

func bpObjects() [][]byte {
	return [][]byte{
		carvetest.BPlistDict([]byte{1}, []byte{2}),
		carvetest.BPlistASCII("name"),
		carvetest.BPlistInt1(42),
	}
}

func TestBPlistValidatesBuilderVariants(t *testing.T) {
	for name, c := range map[string]struct {
		objs   [][]byte
		top    int
		os, rs byte
		want   string
	}{
		"1-byte offsets":       {bpObjects(), 0, 1, 1, "objects=3"},
		"2-byte offsets":       {bpObjects(), 0, 2, 1, "objects=3"},
		"8-byte offsets":       {bpObjects(), 0, 8, 2, "objects=3"},
		"scalar top":           {[][]byte{carvetest.BPlistInt1(7)}, 0, 1, 1, "objects=1"},
		"top is the last":      {bpObjects(), 2, 1, 1, "objects=3"},
		"null and bool marker": {[][]byte{{0x08}, {0x09}, {0x00}}, 1, 1, 1, "objects=3"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.BPlist(c.objs, c.top, c.os, c.rs)
			// Fill after the object is excluded: the first trailer wins.
			v := bplistVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != int64(len(obj)) || !slices.Contains(v.Basis, c.want) {
				t.Fatalf("verdict = %+v, want validated length %d with %s", v, len(obj), c.want)
			}
		})
	}
}

func TestBPlistRejects(t *testing.T) {
	good := carvetest.BPlist(bpObjects(), 0, 1, 1)
	trailer := len(good) - 32
	tableOff := trailer - 3 // three 1-byte entries
	patch := func(f func(b []byte)) []byte { b := slices.Clone(good); f(b); return b }
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"top object == N", patch(func(b []byte) { b[trailer+23] = 3 }), "no-trailer"},
		{"top object > N", patch(func(b []byte) { b[trailer+23] = 200 }), "no-trailer"},
		{"entry points into the trailer", patch(func(b []byte) { b[tableOff+1] = byte(trailer + 4) }), "no-trailer"},
		{"entry points into the header", patch(func(b []byte) { b[tableOff+1] = 3 }), "no-trailer"},
		{"entry on a bad marker 0xB0", patch(func(b []byte) { b[b[tableOff+1]] = 0xB0 }), "no-trailer"},
		{"entry on a bad marker 0x70", patch(func(b []byte) { b[b[tableOff+2]] = 0x70 }), "no-trailer"},
		{"offset size 0", patch(func(b []byte) { b[trailer+6] = 0 }), "no-trailer"},
		{"offset size 9", patch(func(b []byte) { b[trailer+6] = 9 }), "no-trailer"},
		{"ref size 0", patch(func(b []byte) { b[trailer+7] = 0 }), "no-trailer"},
		{"ref size 9", patch(func(b []byte) { b[trailer+7] = 9 }), "no-trailer"},
		{"table offset 8", patch(func(b []byte) { b[trailer+31] = 8 }), "no-trailer"},
		{"count 0", patch(func(b []byte) { b[trailer+15] = 0 }), "no-trailer"},
		{"huge count", patch(func(b []byte) { b[trailer+8] = 0xFF }), "no-trailer"},
		{"table does not end at the trailer", patch(func(b []byte) { b[trailer+31]-- }), "no-trailer"},
		{"a stray byte between table and trailer", slices.Concat(good[:trailer], []byte{0x99}, good[trailer:]), "no-trailer"},
		{"no trailer at all", slices.Concat([]byte("bplist00"), bytes.Repeat([]byte{0x55}, 200)), "no-trailer"},
		{"too short", []byte("bplist00\x08\x00"), "no-trailer"},
		{"bad signature", patch(func(b []byte) { b[7] = '1' }), "bad-signature"},
		{"cut before the trailer ends", good[:len(good)-1], "no-trailer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateBPlist(bplistWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := bplistVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestBPlistWindowBoundsAndBudget: a huge declared count is never read past
// the window; a trailer search that outruns the budget, and a cancelled
// context, are rejected "unreadable" (ruling 3E-C1).
func TestBPlistWindowBoundsAndBudget(t *testing.T) {
	good := carvetest.BPlist(bpObjects(), 0, 1, 1)
	// 1 MiB of 0x01 bytes: every end passes the size-byte filter, none has a
	// consistent trailer.
	buf := slices.Concat([]byte("bplist00"), bytes.Repeat([]byte{1}, 1<<20))
	b := newBudget(1 << 30)
	if v := validateBPlist(bplistWindow(buf, b)); v.Outcome != Rejected || v.Reason != "no-trailer" {
		t.Fatalf("verdict = %+v", v)
	}
	b = newBudget(128 << 10)
	v := validateBPlist(bplistWindow(buf, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(good), 0, int64(len(good)), newBudget(1<<30))
	if v := validateBPlist(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

// TestBPlistReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// signature, in the trailer search and in the offset table is rejected
// "unreadable" (never "truncated" or "no-trailer") and recorded.
func TestBPlistReadErrorIsUnreadable(t *testing.T) {
	// Many objects so the offset table is 600 bytes and the failure can hit it.
	var objs [][]byte
	for range 300 {
		objs = append(objs, carvetest.BPlistInt1(1))
	}
	obj := carvetest.BPlist(objs, 0, 2, 1)
	big := slices.Concat([]byte("bplist00"), bytes.Repeat([]byte{1}, 100000))
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at signature", obj, 0},
		{"mid search", big, 70000},
		{"in the trailer", obj, int64(len(obj)) - 20},
		{"in the offset table", obj, int64(len(obj)) - 32 - 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateBPlist(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
				t.Fatalf("verdict = %+v readErr = %v, want rejected unreadable", v, w.readErr)
			}
		})
	}
}

func TestBPlistRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family string
			Size         int64
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "bplist" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := bplistVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
				t.Fatalf("verdict = %+v, want validated length %d", v, o.Size)
			}
			// The object count is the trailer's own, read here independently.
			var count uint64
			for _, b := range obj[len(obj)-24 : len(obj)-16] {
				count = count<<8 | uint64(b)
			}
			if want := "objects=" + strconv.FormatUint(count, 10); count == 0 || !slices.Contains(v.Basis, want) {
				t.Errorf("basis = %v, want %s", v.Basis, want)
			}
		})
	}
	if n < 1 {
		t.Fatal("no bplist object in the expectations")
	}
}

// TestBPlistCheckGuardsOffsetSize: bplistCheck refuses an offset size outside
// 1..8 itself, whatever its caller filtered (defence in depth).
func TestBPlistCheckGuardsOffsetSize(t *testing.T) {
	good := carvetest.BPlist(bpObjects(), 0, 1, 1)
	trailer := int64(len(good) - 32)
	w := bplistWindow(good, newBudget(1<<30))
	if _, ok, err := bplistCheck(w, good[trailer:], 1, trailer); !ok || err != nil {
		t.Fatalf("control: ok = %v err = %v", ok, err)
	}
	for _, os := range []int64{-1, 0, 9, 1 << 40} {
		if _, ok, err := bplistCheck(w, good[trailer:], os, trailer); ok || err != nil {
			t.Errorf("offset size %d: ok = %v err = %v, want refused", os, ok, err)
		}
	}
}
