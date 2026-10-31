package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func oggVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateOgg(riffWindow(buf, newBudget(1<<30)))
}

func seg(n int) [][]byte { return [][]byte{bytes.Repeat([]byte{0xA5}, n)} }

func oneStream() []carvetest.OggPage {
	return []carvetest.OggPage{
		{Flags: 2, Serial: 7, Seq: 0, Segments: seg(30)},
		{Serial: 7, Seq: 1, Segments: seg(100)},
		{Flags: 4, Serial: 7, Seq: 2, Segments: seg(10)},
	}
}

func TestOggValidatesBuilderVariants(t *testing.T) {
	long := bytes.Repeat([]byte{0xA5}, 255)
	for name, c := range map[string]struct {
		pages           []carvetest.OggPage
		streams, npages string
	}{
		"one stream": {oneStream(), "1", "3"},
		"one page": {[]carvetest.OggPage{
			{Flags: 6, Serial: 1, Segments: seg(5)},
		}, "1", "1"},
		"two multiplexed streams": {[]carvetest.OggPage{
			{Flags: 2, Serial: 1, Segments: seg(30)},
			{Flags: 2, Serial: 2, Segments: seg(40)},
			{Serial: 1, Seq: 1, Segments: seg(10)},
			{Flags: 4, Serial: 1, Seq: 2, Segments: seg(10)},
			{Serial: 2, Seq: 1, Segments: seg(10)},
			{Flags: 4, Serial: 2, Seq: 2},
		}, "2", "6"},
		"continued packet across pages": {[]carvetest.OggPage{
			{Flags: 2, Serial: 9, Segments: [][]byte{long, long}},
			{Flags: 1, Serial: 9, Seq: 1, Segments: [][]byte{long, long[:40]}},
			{Flags: 4, Serial: 9, Seq: 2, Segments: seg(3)},
		}, "1", "3"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.Ogg(c.pages)
			for _, trail := range []int{0, 4096} {
				v := oggVerdict(obj, trail)
				if v.Outcome != Validated || v.Length != int64(len(obj)) {
					t.Fatalf("trail %d: verdict = %+v, want validated length %d", trail, v, len(obj))
				}
				if want := []string{"streams=" + c.streams, "pages=" + c.npages}; !slices.Equal(v.Basis, want) {
					t.Errorf("basis = %v, want %v", v.Basis, want)
				}
			}
		})
	}
}

// TestOggEndsAtTheFirstClosedGroup: a chained second stream after the first
// one ended is not part of the object.
func TestOggEndsAtTheFirstClosedGroup(t *testing.T) {
	first := carvetest.Ogg(oneStream())
	second := carvetest.Ogg([]carvetest.OggPage{{Flags: 6, Serial: 99, Segments: seg(5)}})
	if v := oggVerdict(slices.Concat(first, second), 0); v.Outcome != Validated || v.Length != int64(len(first)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(first))
	}
}

func TestOggRejects(t *testing.T) {
	good := carvetest.Ogg(oneStream())
	mod := func(f func(p []carvetest.OggPage) []carvetest.OggPage) []byte {
		return carvetest.Ogg(f(oneStream()))
	}
	withByte := func(i int, b byte) []byte { c := slices.Clone(good); c[i] = b; return c }
	var big [][]byte
	for range 255 {
		big = append(big, bytes.Repeat([]byte{1}, 255))
	}
	manyStreams := make([]carvetest.OggPage, 0, 2000)
	for i := range 2000 {
		manyStreams = append(manyStreams, carvetest.OggPage{Flags: 2, Serial: uint32(i), Segments: seg(1)})
	}
	twoPages := len(carvetest.Ogg(oneStream()[:2]))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"BadCRC", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[1].BadCRC = true; return p }), "crc"},
		{"BadCRC on the first page", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[0].BadCRC = true; return p }), "crc"},
		{"first page without BOS", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[0].Flags = 0; return p }), "no-bos"},
		{"missing EOS page", mod(func(p []carvetest.OggPage) []carvetest.OggPage { return p[:2] }), "no-eos"},
		{"missing EOS, other bytes follow", slices.Concat(good[:twoPages], bytes.Repeat(fillPattern, 10)), "no-eos"},
		{"version 1", withByte(4, 1), "bad-page"},
		{"reserved flag bit", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[1].Flags = 8; return p }), "bad-page"},
		{"page of a serial without BOS", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[1].Serial = 8; return p }), "bad-page"},
		{"BOS repeated for a begun serial", mod(func(p []carvetest.OggPage) []carvetest.OggPage { p[1].Flags = 2; return p }), "bad-page"},
		{"page after its stream's EOS", carvetest.Ogg([]carvetest.OggPage{
			{Flags: 2, Serial: 7, Segments: seg(4)},
			{Flags: 2, Serial: 5, Segments: seg(4)},
			{Flags: 4, Serial: 7, Seq: 1},
			{Serial: 7, Seq: 2},
		}), "bad-page"},
		{"page cut short", good[:len(good)-5], "truncated"},
		{"header cut short", good[:twoPages+10], "truncated"},
		{"page past the window", carvetest.Ogg([]carvetest.OggPage{{Flags: 2, Serial: 1, Segments: big}})[:20000], "truncated"},
		{"too many streams", carvetest.Ogg(manyStreams), "too-many-streams"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateOgg(riffWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
}

// TestOggWindowBoundsAndReadErrors: a read error or a spent budget is
// "unreadable" (ruling 3E-C1) and a long chain of pages is stopped by the budget.
func TestOggWindowBoundsAndReadErrors(t *testing.T) {
	obj := carvetest.Ogg(oneStream())
	for _, at := range []int64{0, 30, 100, int64(len(obj)) - 12} {
		w := newWindow(context.Background(), failAt{obj, at}, 0, int64(len(obj)), newBudget(1<<30))
		if v := validateOgg(w); v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
			t.Fatalf("at %d: verdict = %+v read error = %v", at, v, w.readErr)
		}
	}
	pages := []carvetest.OggPage{{Flags: 2, Serial: 1, Segments: seg(1)}}
	for i := range 5000 {
		pages = append(pages, carvetest.OggPage{Serial: 1, Seq: uint32(i + 1), Segments: seg(1)})
	}
	long := carvetest.Ogg(pages)
	b := newBudget(8 << 10)
	if v := validateOgg(riffWindow(long, b)); v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted {
		t.Fatalf("budget verdict = %+v exhausted = %v", v, b.exhausted)
	}
	if v := validateOgg(riffWindow(long, newBudget(1<<30))); v.Outcome != Rejected || v.Reason != "no-eos" {
		t.Fatalf("walk verdict = %+v, want no-eos", v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v := validateOgg(newWindow(ctx, bytes.NewReader(long), 0, int64(len(long)), newBudget(1<<30))); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

func TestOggRealEncoderObjects(t *testing.T) {
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
		if o.Family != "ogg" {
			continue
		}
		n++
		obj, err := os.ReadFile(filepath.Join(dir, o.File))
		if err != nil {
			t.Fatal(err)
		}
		v := oggVerdict(obj, 4096)
		if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
			t.Fatalf("%s: verdict = %+v, want validated length %d", o.File, v, o.Size)
		}
		if !slices.Contains(v.Basis, "streams=1") {
			t.Errorf("%s: basis = %v, want streams=1", o.File, v.Basis)
		}
	}
	if n < 1 {
		t.Fatal("no ogg object in the expectations")
	}
}
