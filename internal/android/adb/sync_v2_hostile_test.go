package adb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
)

// dnt2 builds a DNT2 frame directly from the layout (not through adbtest).
func dnt2(errno, mode uint32, size uint64, mtime int64, namelen uint32, name string) []byte {
	b := []byte("DNT2")
	b = binary.LittleEndian.AppendUint32(b, errno)
	b = append(b, make([]byte, 16)...) // dev, ino
	b = binary.LittleEndian.AppendUint32(b, mode)
	b = append(b, make([]byte, 12)...) // nlink, uid, gid
	b = binary.LittleEndian.AppendUint64(b, size)
	b = binary.LittleEndian.AppendUint64(b, uint64(mtime))
	b = binary.LittleEndian.AppendUint64(b, uint64(mtime))
	b = binary.LittleEndian.AppendUint64(b, uint64(mtime))
	b = binary.LittleEndian.AppendUint32(b, namelen)
	return append(b, name...)
}

func done2(errno uint32) []byte {
	b := append([]byte("DONE"), make([]byte, 72)...)
	binary.LittleEndian.PutUint32(b[4:], errno)
	return b
}

func hostileSyncV2(t *testing.T, reply func(w io.Writer)) *adb.Sync {
	t.Helper()
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) == nil {
			reply(c)
		}
		_, _ = io.Copy(io.Discard, c)
	})
	s, err := c.SyncWith(context.Background(), "H1", adb.SyncV2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSyncV2ListHostileFrames(t *testing.T) {
	long := string(bytes.Repeat([]byte{'n'}, 4097))
	for _, tc := range []struct {
		name  string
		reply func(io.Writer)
	}{
		{"name length over the cap", func(w io.Writer) { _, _ = w.Write(dnt2(0, 0o100644, 1, 0, 4097, long)) }},
		{"size over int64", func(w io.Writer) { _, _ = w.Write(dnt2(0, 0o100644, 1<<63, 0, 1, "a")) }},
	} {
		s := hostileSyncV2(t, tc.reply)
		if _, err := s.ListN("/sdcard", 0); !errors.Is(err, adb.ErrProtocol) {
			t.Errorf("%s: err = %v, want ErrProtocol", tc.name, err)
		}
	}
	// A frame cut short and then a closed connection is an error, never a result.
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) == nil {
			_, _ = c.Write(dnt2(0, 0, 0, 0, 1, "a")[:40])
		}
	})
	s, err := c.SyncWith(context.Background(), "H1", adb.SyncV2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if res, err := s.ListN("/sdcard", 0); err == nil {
		t.Fatalf("a cut-short DNT2 frame returned %+v", res)
	}
}

func TestSyncV2ListErrorsAreNotEntries(t *testing.T) {
	s := hostileSyncV2(t, func(w io.Writer) {
		_, _ = w.Write(dnt2(0, 0o040755, 0, 0, 1, "."))
		_, _ = w.Write(dnt2(0, 0o040755, 0, 0, 2, ".."))
		_, _ = w.Write(dnt2(0, 0o100644, 5, 1700000000, 2, "ok"))
		_, _ = w.Write(dnt2(13, 0, 0, 0, 3, "bad")) // EACCES
		_, _ = w.Write(done2(0))
	})
	res, err := s.ListN("/sdcard", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "ok" || len(res.EntryErrors) != 1 || res.EntryErrors[0] != (adb.EntryError{Name: "bad", Errno: 13}) {
		t.Fatalf("entries = %+v, errors = %+v", res.Entries, res.EntryErrors)
	}
	if e := res.Err("/sdcard"); e == nil || !strings.Contains(e.Error(), "bad") {
		t.Fatalf("Err = %v: an entry the device could not stat must surface", e)
	}
}

func TestSyncV2DirectoryErrnoIsNotAnEmptyDirectory(t *testing.T) {
	s := hostileSyncV2(t, func(w io.Writer) { _, _ = w.Write(done2(13)) })
	res, err := s.ListN("/data", 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Err("/data"); !errors.Is(e, fs.ErrPermission) {
		t.Fatalf("Err = %v, want a permission error", e)
	}
}

func TestListWithoutAnyEntryIsUnknownNotEmpty(t *testing.T) {
	for name, mode := range map[string]adb.SyncMode{"v1": adb.SyncV1, "v2": adb.SyncV2} {
		c := hostileServer(t, func(c net.Conn) {
			if readSyncRequest(c) == nil {
				if mode == adb.SyncV1 {
					_, _ = c.Write(frame("DONE", 0, 0, 0, 0))
				} else {
					_, _ = c.Write(done2(0))
				}
			}
			_, _ = io.Copy(io.Discard, c)
		})
		s, err := c.SyncWith(context.Background(), "H1", mode)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.ListN("/data", 0)
		_ = s.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if e := res.Err("/data"); !errors.Is(e, adb.ErrListUnknown) {
			t.Errorf("%s: Err = %v, want ErrListUnknown", name, e)
		}
	}
}

func TestListWithOnlyDotsIsAGenuinelyEmptyDirectory(t *testing.T) {
	srv, err := adbtest.Start(&adbtest.Device{
		Serial: "E1", State: "device",
		Files:     map[string]adbtest.File{"/sdcard/a": {Data: []byte("x")}},
		EmptyDirs: map[string]bool{"/sdcard/empty": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	for name, mode := range map[string]adb.SyncMode{"v1": adb.SyncV1, "v2": adb.SyncV2} {
		s, err := adb.New(srv.Addr()).SyncWith(t.Context(), "E1", mode)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.ListN("/sdcard/empty", 0)
		if err != nil || len(res.Entries) != 0 || res.Raw != 2 || res.Err("/sdcard/empty") != nil {
			t.Errorf("%s: res = %+v, err = %v", name, res, err)
		}
		_ = s.Close()
	}
}

func TestFakeDeviceSignalsUnreadableDirectories(t *testing.T) {
	srv, err := adbtest.Start(&adbtest.Device{
		Serial: "U1", State: "device",
		Files:      map[string]adbtest.File{"/sdcard/a": {Data: []byte("x")}, "/sdcard/b": {Data: []byte("y")}},
		ListErrno:  map[string]uint32{"/data": 13},
		EntryErrno: map[string]uint32{"/sdcard/b": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	v2, err := adb.New(srv.Addr()).Sync(t.Context(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()
	res, err := v2.ListN("/data", 0)
	if err != nil || !errors.Is(res.Err("/data"), fs.ErrPermission) {
		t.Fatalf("v2 /data: %+v, %v", res, err)
	}
	res, err = v2.ListN("/sdcard", 0)
	if err != nil || len(res.Entries) != 1 || len(res.EntryErrors) != 1 || res.EntryErrors[0].Errno != 5 {
		t.Fatalf("v2 /sdcard: %+v, %v", res, err)
	}
	if _, err := v2.Stat("/sdcard/b"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a stat that fails with EIO must not read as missing: %v", err)
	}
	v1, err := adb.New(srv.Addr()).SyncWith(t.Context(), "U1", adb.SyncV1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v1.Close() }()
	res, err = v1.ListN("/data", 0)
	if err != nil || !errors.Is(res.Err("/data"), adb.ErrListUnknown) {
		t.Fatalf("v1 /data (a bare DONE): %+v, %v", res, err)
	}
}

func TestSyncV2StatErrnoIsNotMissing(t *testing.T) {
	s := hostileSyncV2(t, func(w io.Writer) {
		b := []byte("STA2")
		b = binary.LittleEndian.AppendUint32(b, 13)
		_, _ = w.Write(append(b, make([]byte, 64)...))
	})
	_, err := s.Stat("/data/x")
	var ee *adb.ErrnoError
	if !errors.As(err, &ee) || ee.Errno != 13 || errors.Is(err, fs.ErrNotExist) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}
