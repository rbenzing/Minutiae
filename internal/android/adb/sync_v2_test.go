package adb_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
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
	s, err := c.SyncWith(t.Context(), "H1", adb.SyncV1)
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
	s, err := c.SyncWith(t.Context(), "H1", adb.SyncV1)
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
	s, err := c.SyncWith(t.Context(), "H1", adb.SyncV1)
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

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// The documented layout of a sync v2 directory entry, field by field. This is
// from memory of the adb sync protocol notes (no real adbd capture is in the
// repo): id, error, dev, ino, mode, nlink, uid, gid, size, atime, mtime,
// ctime, namelen, then the name; all little-endian; 76 bytes before the name.
func TestV2FrameLayout(t *testing.T) {
	want := bytes.Join([][]byte{
		[]byte("DNT2"),
		le32(0),          // error (errno)
		le64(0),          // dev
		le64(0),          // ino
		le32(0x81A4),     // mode
		le32(1),          // nlink
		le32(0), le32(0), // uid, gid
		le64(0x100000005), // size: 4 GiB + 5
		le64(1700000000),  // atime
		le64(1700000000),  // mtime
		le64(1700000000),  // ctime
		le32(4),           // namelen
		[]byte("a.db"),
	}, nil)
	if len(want) != 76+4 {
		t.Fatalf("layout table is %d bytes", len(want))
	}
	got := adbtest.EncodeDNT2(0, 0x81A4, 0x100000005, 1700000000, "a.db")
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeDNT2 =\n% x\nwant\n% x", got, want)
	}
}

func TestFeaturesQuery(t *testing.T) {
	modern := &adbtest.Device{Serial: "M1", State: "device"}
	legacy := &adbtest.Device{Serial: "L1", State: "device", Features: []string{"shell_v2"}}
	srv, err := adbtest.Start(modern, legacy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c := adb.New(srv.Addr())
	got, err := c.Features(t.Context(), "M1")
	if err != nil || !slices.Contains(got, "ls_v2") || !slices.Contains(got, "stat_v2") {
		t.Fatalf("modern features = %v, %v", got, err)
	}
	got, err = c.Features(t.Context(), "L1")
	if err != nil || slices.Contains(got, "ls_v2") || !slices.Contains(got, "shell_v2") {
		t.Fatalf("legacy features = %v, %v", got, err)
	}
	if _, err := c.Features(t.Context(), "nope"); err == nil {
		t.Fatal("features of an unknown device succeeded")
	}
}

func TestSyncNegotiation(t *testing.T) {
	devs := []*adbtest.Device{
		{Serial: "M1", State: "device"},
		{Serial: "L1", State: "device", Features: []string{"shell_v2"}},
		{Serial: "H1", State: "device", Features: []string{"stat_v2"}},
	}
	srv, err := adbtest.Start(devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c := adb.New(srv.Addr())
	for _, tc := range []struct {
		serial  string
		mode    adb.SyncMode
		version int
		reason  string
	}{
		{"M1", adb.SyncAuto, 2, ""},
		{"L1", adb.SyncAuto, 1, "ls_v2"},
		{"H1", adb.SyncAuto, 1, "ls_v2"}, // both are needed
		{"M1", adb.SyncV1, 1, "v1 requested"},
	} {
		s, err := c.SyncWith(t.Context(), tc.serial, tc.mode)
		if err != nil {
			t.Fatal(err)
		}
		if s.Version() != tc.version || !strings.Contains(s.FallbackReason(), tc.reason) || (tc.version == 2) != (s.FallbackReason() == "") {
			t.Errorf("%s mode %d: version %d, reason %q", tc.serial, tc.mode, s.Version(), s.FallbackReason())
		}
		_ = s.Close()
	}
}

// A server that refuses the features query degrades to v1 and says why.
func TestSyncNegotiationQueryRefusedFallsBackToV1(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				for {
					req, err := readHostRequest(c)
					if err != nil {
						return
					}
					if strings.HasSuffix(req, ":features") {
						msg := "unknown host service"
						_, _ = fmt.Fprintf(c, "FAIL%04x%s", len(msg), msg)
						return
					}
					_, _ = c.Write([]byte("OKAY")) // host:transport:<serial>, then sync:
				}
			}()
		}
	}()
	s, err := adb.New(ln.Addr().String()).Sync(t.Context(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Version() != 1 || !strings.Contains(s.FallbackReason(), "feature query refused") || !strings.Contains(s.FallbackReason(), "unknown host service") {
		t.Fatalf("version %d, reason %q", s.Version(), s.FallbackReason())
	}
}

func v2Device() *adbtest.Device {
	return &adbtest.Device{
		Serial: "V1", State: "device",
		Files: map[string]adbtest.File{
			"/sdcard/small.txt": {Data: []byte("hello"), MTime: time.Unix(1700000000, 0)},
			"/sdcard/huge.bin":  {Data: []byte("x"), ListedSize: 5<<30 + 7}, // the listing claims 5 GiB + 7
		},
	}
}

func TestSyncV2ListAndStatCarry64BitSizes(t *testing.T) {
	srv, err := adbtest.Start(v2Device())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	s, err := adb.New(srv.Addr()).Sync(t.Context(), "V1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Version() != 2 {
		t.Fatalf("version = %d (%s)", s.Version(), s.FallbackReason())
	}
	res, err := s.ListN("/sdcard", 0)
	if err != nil || len(res.Entries) != 2 || res.Entries[0].Name != "huge.bin" || res.Entries[0].Size != 5<<30+7 || res.Entries[1].Size != 5 {
		t.Fatalf("list = %+v, %v", res.Entries, err)
	}
	if res.Entries[1].MTime.Unix() != 1700000000 || res.Entries[1].MTimeUnknown {
		t.Fatalf("mtime = %v unknown=%v", res.Entries[1].MTime, res.Entries[1].MTimeUnknown)
	}
	st, err := s.Stat("/sdcard/huge.bin")
	if err != nil || st.Size != 5<<30+7 || !st.IsRegular() {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if _, err := s.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing stat err = %v", err)
	}
}

// The same device through v1: the size is the low 32 bits. That truncation is
// the documented limitation; the walker never records such a size (Task 12).
func TestSyncV1TruncatesSizesTo32Bits(t *testing.T) {
	srv, err := adbtest.Start(v2Device())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	s, err := adb.New(srv.Addr()).SyncWith(t.Context(), "V1", adb.SyncV1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	res, err := s.ListN("/sdcard", 0)
	if err != nil || res.Entries[0].Name != "huge.bin" || res.Entries[0].Size != (5<<30+7)&0xffffffff {
		t.Fatalf("v1 list = %+v, %v", res.Entries, err)
	}
}

func TestSyncEntriesCarryTheVersionOfTheSessionThatListedThem(t *testing.T) {
	srv, err := adbtest.Start(v2Device())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	for mode, want := range map[adb.SyncMode]bool{adb.SyncV1: false, adb.SyncV2: true} {
		s, err := adb.New(srv.Addr()).SyncWith(t.Context(), "V1", mode)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.ListN("/sdcard", 0)
		if err != nil || len(res.Entries) != 2 || res.Entries[0].V2 != want || res.Entries[1].V2 != want {
			t.Errorf("mode %d: entries = %+v, err = %v; want V2 = %v", mode, res.Entries, err, want)
		}
		st, err := s.Stat("/sdcard/small.txt")
		if err != nil || st.V2 != want {
			t.Errorf("mode %d: stat = %+v, err = %v; want V2 = %v", mode, st, err, want)
		}
		_ = s.Close()
	}
}
