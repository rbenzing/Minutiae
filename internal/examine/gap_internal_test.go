package examine

import (
	"math"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/image"
)

// fakeGapImage is an image that declares the given acquisition gaps; only the
// GapReporter method is used by gapNote.
type fakeGapImage struct {
	image.Image
	g image.AcquisitionGaps
}

func (f fakeGapImage) AcquisitionGaps() image.AcquisitionGaps { return f.g }

func gapSession(g image.AcquisitionGaps) *Session { return &Session{Image: fakeGapImage{g: g}} }

func declared(kept ...image.AcquisitionGap) image.AcquisitionGaps {
	return image.AcquisitionGaps{State: image.GapsDeclared, Ranges: uint64(len(kept)), Kept: kept}
}

func TestGapNoteBounds(t *testing.T) {
	t.Run("exact overlap of disjoint runs", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 90, Length: 20}))
		runs := []evidence.Run{{Offset: 0, Length: 100}, {Offset: 100, Length: 100}} // adjacent, not overlapping
		n := s.gapNote(runs, true, 200)
		if n == nil || n.Bytes != 20 || n.Ranges != 2 || n.Unknown {
			t.Fatalf("note = %+v", n)
		}
	})
	t.Run("overlap larger than the artifact is capped and unknown", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 0, Length: 200}))
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: 200}}, true, 100) // runs claim more than the artifact holds
		if n == nil || n.Bytes != 100 || !n.Unknown {
			t.Fatalf("note = %+v, want Bytes 100 and Unknown", n)
		}
	})
	t.Run("overlapping runs make the count unknown, never a double count", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 60, Length: 10}))
		// the two runs overlap on 50..100; the gap lies inside the overlap
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: 100}, {Offset: 50, Length: 100}}, true, 200)
		if n == nil || !n.Unknown || n.Bytes != 10 || n.Ranges != 1 {
			t.Fatalf("note = %+v, want Unknown with the gap counted once (10 bytes, 1 range)", n)
		}
	})
	t.Run("overlapping runs that miss every gap are still unknown", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 500, Length: 10}))
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: 100}, {Offset: 0, Length: 100}}, true, 200)
		if n == nil || !n.Unknown {
			t.Fatalf("note = %+v, want Unknown", n)
		}
	})
	t.Run("a run inside another run", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 20, Length: 10}))
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: 100}, {Offset: 10, Length: 50}}, true, 200)
		if n == nil || !n.Unknown || n.Bytes != 10 || n.Ranges != 1 {
			t.Fatalf("note = %+v", n)
		}
	})
	t.Run("the byte total saturates at MaxInt64", func(t *testing.T) {
		// Kept ranges that overlap each other cannot come from a real container;
		// the sum must still not wrap.
		s := gapSession(declared(image.AcquisitionGap{Offset: 0, Length: math.MaxInt64}, image.AcquisitionGap{Offset: 0, Length: 1}))
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: math.MaxInt64}}, true, math.MaxInt64)
		if n == nil || n.Bytes != math.MaxInt64 || !n.Unknown {
			t.Fatalf("note = %+v, want Bytes MaxInt64 and Unknown", n)
		}
	})
	t.Run("a run whose end overflows is unknown", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 0, Length: 10}))
		n := s.gapNote([]evidence.Run{{Offset: math.MaxInt64 - 1, Length: 10}}, true, 10)
		if n == nil || !n.Unknown {
			t.Fatalf("note = %+v", n)
		}
	})
	t.Run("the pair bound makes the overlap unknown", func(t *testing.T) {
		old := maxGapWork
		maxGapWork = 3
		t.Cleanup(func() { maxGapWork = old })
		kept := make([]image.AcquisitionGap, 10)
		for i := range kept {
			kept[i] = image.AcquisitionGap{Offset: int64(i) * 10, Length: 5}
		}
		s := gapSession(declared(kept...))
		n := s.gapNote([]evidence.Run{{Offset: 0, Length: 100}}, true, 100)
		if n == nil || !n.Unknown {
			t.Fatalf("note = %+v, want Unknown at the bound", n)
		}
		// exactly at the bound is still exact
		maxGapWork = 10
		n = s.gapNote([]evidence.Run{{Offset: 0, Length: 100}}, true, 100)
		if n == nil || n.Unknown || n.Ranges != 10 || n.Bytes != 50 {
			t.Fatalf("note at the bound = %+v", n)
		}
	})
	t.Run("a truncated range list is unknown but counts what it keeps", func(t *testing.T) {
		g := declared(image.AcquisitionGap{Offset: 0, Length: 10})
		g.Truncated = true
		n := gapSession(g).gapNote([]evidence.Run{{Offset: 0, Length: 100}}, true, 100)
		if n == nil || !n.Unknown || n.Bytes != 10 {
			t.Fatalf("note = %+v", n)
		}
	})
	t.Run("a damaged error2 section is unknown for every non-empty artifact", func(t *testing.T) {
		n := gapSession(image.AcquisitionGaps{State: image.GapsDamaged}).gapNote([]evidence.Run{{Offset: 0, Length: 10}}, true, 10)
		if n == nil || !n.Unknown {
			t.Fatalf("note = %+v", n)
		}
	})
	t.Run("unrecorded runs are unknown, an empty artifact has no note", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 0, Length: 10}))
		if n := s.gapNote(nil, false, 10); n == nil || !n.Unknown {
			t.Fatalf("unrecorded runs: %+v", n)
		}
		if n := s.gapNote([]evidence.Run{{Offset: 0, Length: 10}}, true, 0); n != nil {
			t.Fatalf("empty artifact: %+v", n)
		}
	})
	t.Run("holes carry no image bytes", func(t *testing.T) {
		s := gapSession(declared(image.AcquisitionGap{Offset: 0, Length: 10}))
		if n := s.gapNote([]evidence.Run{{Offset: -1, Length: 10}, {Offset: 100, Length: 10}}, true, 20); n != nil {
			t.Fatalf("note = %+v", n)
		}
	})
}

// The session's writer rule is gapNoteFor itself: one function, so the verify recompute (R10) and the
// writers cannot disagree. A session whose image reports no gaps writes no note.
func TestGapNoteForEqualsTheWriter(t *testing.T) {
	g := declared(image.AcquisitionGap{Offset: 90, Length: 20}, image.AcquisitionGap{Offset: 300, Length: 5})
	for _, tc := range []struct {
		name  string
		runs  []evidence.Run
		known bool
		size  int64
	}{
		{"disjoint runs", []evidence.Run{{Offset: 0, Length: 100}, {Offset: 100, Length: 100}}, true, 200},
		{"clamp", []evidence.Run{{Offset: 0, Length: 400}}, true, 10},
		{"overlapping runs", []evidence.Run{{Offset: 0, Length: 100}, {Offset: 50, Length: 100}}, true, 400},
		{"runs not recorded", nil, false, 200},
		{"empty artifact", []evidence.Run{{Offset: 0, Length: 100}}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gapSession(g).gapNote(tc.runs, tc.known, tc.size)
			if want := gapNoteFor(g, tc.runs, tc.known, tc.size); !reflect.DeepEqual(got, want) {
				t.Fatalf("session note %+v, gapNoteFor %+v", got, want)
			}
		})
	}
	if n := (&Session{Image: nil}).gapNote([]evidence.Run{{Offset: 0, Length: 10}}, true, 10); n != nil {
		t.Fatalf("a session without a gap reporter wrote %+v", n)
	}
}
