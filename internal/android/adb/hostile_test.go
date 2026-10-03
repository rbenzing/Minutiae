package adb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb"
)

// hostileServer is a minimal raw ADB server: it accepts host:transport:<any>
// and the following service request, then hands the connection to serve,
// which writes whatever (malicious) frames the test needs.
func hostileServer(t *testing.T, serve func(c net.Conn)) *adb.Client {
	t.Helper()
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
				for range 2 { // host:transport:<serial>, then the service
					if _, err := readHostRequest(c); err != nil {
						return
					}
					_, _ = c.Write([]byte("OKAY"))
				}
				serve(c)
			}()
		}
	}()
	return adb.New(ln.Addr().String())
}

func readHostRequest(r io.Reader) (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(l[:]), 16, 16)
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

// readSyncRequest consumes one sync request (id, length, argument).
func readSyncRequest(r io.Reader) error {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, r, int64(binary.LittleEndian.Uint32(h[4:])))
	return err
}

func frame(id string, vals ...uint32) []byte {
	b := []byte(id)
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return b
}

func hostileSync(t *testing.T, reply func(w io.Writer)) *adb.Sync {
	t.Helper()
	c := hostileServer(t, func(c net.Conn) {
		if readSyncRequest(c) == nil {
			reply(c)
		}
		_, _ = io.Copy(io.Discard, c) // hold the session open until the client closes it
	})
	s, err := c.Sync(context.Background(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func dent(name string) []byte {
	return append(frame("DENT", 0o100644, 1, 0, uint32(len(name))), name...)
}

func TestSyncListRejectsOversizedDentName(t *testing.T) {
	long := string(bytes.Repeat([]byte{'n'}, 4097))
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(dent(long))
		_, _ = w.Write(frame("DONE", 0, 0, 0, 0))
	})
	es, err := s.List("/sdcard")
	if !errors.Is(err, adb.ErrProtocol) {
		t.Fatalf("4097-byte DENT name: entries=%d err=%v, want ErrProtocol", len(es), err)
	}
}

func TestSyncListAcceptsMaxDentName(t *testing.T) {
	name := string(bytes.Repeat([]byte{'n'}, 4096))
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(dent(name))
		_, _ = w.Write(frame("DONE", 0, 0, 0, 0))
	})
	if es, err := s.List("/sdcard"); err != nil || len(es) != 1 || es[0].Name != name {
		t.Fatalf("4096-byte DENT name: entries=%d err=%v", len(es), err)
	}
}

func TestSyncRejectsOversizedFailMessage(t *testing.T) {
	msg := bytes.Repeat([]byte{'x'}, 64*1024+1)
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(frame("FAIL", uint32(len(msg))))
		_, _ = w.Write(msg)
	})
	_, err := s.Recv("/sdcard/a", io.Discard)
	var fe *adb.FailError
	if !errors.Is(err, adb.ErrProtocol) || errors.As(err, &fe) {
		t.Fatalf("64 KiB+1 FAIL message: err=%.200v, want ErrProtocol and no FailError", err)
	}
}

func TestSyncFailMessageAtCapIsFailError(t *testing.T) {
	msg := bytes.Repeat([]byte{'x'}, 64*1024)
	s := hostileSync(t, func(w io.Writer) {
		_, _ = w.Write(frame("FAIL", uint32(len(msg))))
		_, _ = w.Write(msg)
	})
	_, err := s.Recv("/sdcard/a", io.Discard)
	var fe *adb.FailError
	if !errors.As(err, &fe) || len(fe.Msg) != len(msg) {
		t.Fatalf("64 KiB FAIL message: err=%.200v", err)
	}
}

func TestOutputRejectsOversizedOutput(t *testing.T) {
	const limit = 16 << 20
	for _, tc := range []struct {
		size    int
		wantErr bool
	}{{limit, false}, {limit + 1, true}} {
		c := hostileServer(t, func(c net.Conn) {
			_, _ = c.Write(bytes.Repeat([]byte{'o'}, tc.size))
		})
		out, err := c.Output(context.Background(), "H1", "cat big")
		if gotErr := errors.Is(err, adb.ErrOutputTooLarge); gotErr != tc.wantErr || (!tc.wantErr && (err != nil || len(out) != tc.size)) {
			t.Errorf("%d-byte output: len=%d err=%v", tc.size, len(out), err)
		}
	}
}
