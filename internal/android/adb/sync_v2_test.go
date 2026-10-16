package adb_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
)

// A hostile device streams entries forever and never sends DONE: ListN must
// stop at the limit and return, not read on.
func TestSyncListNStopsAtTheLimitWithoutHanging(t *testing.T) {
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) != nil {
			return
		}
		for i := 0; ; i++ {
			if _, err := c.Write(dent(fmt.Sprintf("f%d", i))); err != nil {
				return
			}
		}
	})
	s, err := c.Sync(t.Context(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	type out struct {
		res adb.ListResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.ListN("/sdcard", 3)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if !errors.Is(o.err, adb.ErrListLimit) || len(o.res.Entries) != 3 || o.res.Entries[2].Name != "f2" {
			t.Fatalf("entries = %+v, err = %v", o.res.Entries, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListN never returned")
	}
}

// "." streamed forever never counts as an entry but is bounded all the same.
func TestSyncListNBoundsRepeatedDotEntries(t *testing.T) {
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) != nil {
			return
		}
		for {
			if _, err := c.Write(dent(".")); err != nil {
				return
			}
		}
	})
	s, err := c.Sync(t.Context(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	done := make(chan error, 1)
	go func() { _, err := s.ListN("/sdcard", 3); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, adb.ErrListLimit) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListN never returned")
	}
}

// W14: a count cap alone would let a device make the client hold gigabytes of
// names. 4096-byte names streamed forever stop at the 64 MiB name budget, far
// below the entry count limit.
func TestSyncListNBoundsNameBytes(t *testing.T) {
	long := string(bytes.Repeat([]byte{'n'}, 4096))
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) != nil {
			return
		}
		for {
			if _, err := c.Write(dent(long)); err != nil {
				return
			}
		}
	})
	s, err := c.Sync(t.Context(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	type out struct {
		res adb.ListResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.ListN("/sdcard", 0)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if !errors.Is(o.err, adb.ErrListLimit) || len(o.res.Entries) >= adb.DefaultMaxListEntries || len(o.res.Entries) > (64<<20)/4096 {
			t.Fatalf("%d entries, err = %v", len(o.res.Entries), o.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ListN never returned")
	}
}

func TestSyncListNExactlyAtTheLimitSucceeds(t *testing.T) {
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(dent("."))
		_, _ = w.Write(dent(".."))
		for _, n := range []string{"a", "b", "c"} {
			_, _ = w.Write(dent(n))
		}
		_, _ = w.Write(frame("DONE", 0, 0, 0, 0))
	})
	res, err := s.ListN("/sdcard", 3)
	if err != nil || len(res.Entries) != 3 || res.Raw != 5 {
		t.Fatalf("entries = %+v raw = %d, err = %v", res.Entries, res.Raw, err)
	}
}

// A caller-supplied limit near MaxInt must be clamped: unclamped, limit+2
// wraps negative and a three-entry directory would be refused as over-limit.
func TestSyncListNClampsAHugeLimit(t *testing.T) {
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(dent("."))
		_, _ = w.Write(dent(".."))
		for _, n := range []string{"a", "b", "c"} {
			_, _ = w.Write(dent(n))
		}
		_, _ = w.Write(frame("DONE", 0, 0, 0, 0))
	})
	res, err := s.ListN("/sdcard", math.MaxInt)
	if err != nil || len(res.Entries) != 3 {
		t.Fatalf("entries = %+v, err = %v", res.Entries, err)
	}
}
