package examine_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestOpenVerifyParentCatchesSameSizeSwap(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, data, 1)
	evidencetest.FlipArtifactByte(t, c.Dir, recs[0], int64(len(data))/2) // same size, different content

	// Default: the cheap size check cannot see it.
	s, err := examine.OpenContainer(c, recs[0].ID, mtfsOpts())
	if err != nil {
		t.Fatalf("default open: %v", err)
	}
	_ = s.Close()

	opts := mtfsOpts()
	opts.VerifyParent = true
	_, err = examine.OpenContainer(c, recs[0].ID, opts)
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("VerifyParent open = %v, want ErrIntegrity", err)
	}
}

func TestOpenVerifyParentAcceptsAnUntouchedParent(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, data, 3) // three segments: every one is re-hashed
	opts := mtfsOpts()
	opts.VerifyParent = true
	var last int64
	opts.VerifyProgress = func(done, _ int64) { last = done }
	s, err := examine.OpenContainer(c, recs[0].ID, opts)
	if err != nil {
		t.Fatalf("VerifyParent open of a clean parent: %v", err)
	}
	_ = s.Close()
	if last == 0 {
		t.Error("no progress was reported while re-hashing")
	}
}

func TestOpenVerifyParentNamesTheBadSegment(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: bytes.Repeat([]byte("x"), 30000)}}}))
	recs := importImage(t, c, data, 3)
	evidencetest.FlipArtifactByte(t, c.Dir, recs[1], 3)
	opts := mtfsOpts()
	opts.VerifyParent = true
	_, err := examine.OpenContainer(c, recs[0].ID, opts)
	if !errors.Is(err, evidence.ErrIntegrity) || !bytes.Contains([]byte(err.Error()), []byte(recs[1].ID)) {
		t.Fatalf("err = %v; want ErrIntegrity naming segment 2 (%s)", err, recs[1].ID)
	}
}

// No verified-stamp: tampering AFTER a successful verified open is seen by the next one.
func TestVerifyParentIsNotCached(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, data, 1)
	opts := mtfsOpts()
	opts.VerifyParent = true
	s, err := examine.OpenContainer(c, recs[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	evidencetest.FlipArtifactByte(t, c.Dir, recs[0], 100)
	if _, err := examine.OpenContainer(c, recs[0].ID, opts); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("second open after tamper = %v, want ErrIntegrity", err)
	}
}

func TestOpenVerifyParentHonoursCancel(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, data, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := mtfsOpts()
	opts.VerifyParent, opts.Context = true, ctx
	_, err := examine.OpenContainer(c, recs[0].ID, opts)
	if !errors.Is(err, context.Canceled) || errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("err = %v, want context.Canceled and not an integrity error", err)
	}
}

// X41: with three segments the progress denominator is the sum over all of them
// from the first report on, and 100% is reached only by the last segment.
func TestOpenVerifyParentProgressSpansThreeSegments(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", Nodes: []fstest.Node{{Path: "/a.txt", Data: bytes.Repeat([]byte("x"), 30000)}}}))
	recs := importImage(t, c, data, 3)
	if len(recs) != 3 {
		t.Fatalf("%d segments, want 3", len(recs))
	}
	var sum int64
	for _, r := range recs {
		sum += r.Size
	}
	type report struct{ done, total int64 }
	var reports []report
	opts := mtfsOpts()
	opts.VerifyParent = true
	opts.VerifyProgress = func(done, total int64) { reports = append(reports, report{done, total}) }
	s, err := examine.OpenContainer(c, recs[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if len(reports) == 0 {
		t.Fatal("no progress reported")
	}
	var prev int64
	for i, r := range reports {
		if r.total != sum || r.done < prev || r.done > sum {
			t.Fatalf("report %d = %+v (previous done %d), want total %d and done rising up to it", i, r, prev, sum)
		}
		if r.done == sum && i != len(reports)-1 {
			t.Fatalf("100%% reached at report %d of %d: %+v", i, len(reports), reports)
		}
		prev = r.done
	}
	if prev != sum {
		t.Fatalf("final done = %d, want %d", prev, sum)
	}
}
