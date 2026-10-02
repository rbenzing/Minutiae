package adb_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
)

var big = bytes.Repeat([]byte{0xAB, 0xCD, 0xEF}, 70_000) // 210 KB, four DATA chunks

func syncDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "S1", State: "device",
		Files: map[string]adbtest.File{
			"/sdcard/a.txt":        {Data: []byte("hello"), MTime: time.Unix(1700000000, 0)},
			"/sdcard/DCIM/big.jpg": {Data: big},
			"/sdcard/secret.db":    {Data: []byte("x")},
		},
		Unreadable: map[string]bool{"/sdcard/secret.db": true},
	}
}

func openSync(t *testing.T) (*adb.Sync, *adbtest.Server) {
	t.Helper()
	s, err := adbtest.Start(syncDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc, err := adb.New(s.Addr()).Sync(context.Background(), "S1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc, s
}

func TestSyncListAndStat(t *testing.T) {
	sc, _ := openSync(t)
	es, err := sc.List("/sdcard")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[0].Name != "DCIM" || !es[0].IsDir() || es[1].Name != "a.txt" || !es[1].IsRegular() || es[1].Size != 5 {
		t.Fatalf("entries = %+v", es)
	}
	st, err := sc.Stat("/sdcard/a.txt")
	if err != nil || st.Size != 5 || st.MTime.Unix() != 1700000000 {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if _, err := sc.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing stat err = %v", err)
	}
}

func TestSyncRecvLarge(t *testing.T) {
	sc, _ := openSync(t)
	var buf bytes.Buffer
	n, err := sc.Recv("/sdcard/DCIM/big.jpg", &buf)
	if err != nil || n != int64(len(big)) || !bytes.Equal(buf.Bytes(), big) {
		t.Fatalf("n=%d err=%v equal=%v", n, err, bytes.Equal(buf.Bytes(), big))
	}
}

func TestSyncRecvFail(t *testing.T) {
	sc, _ := openSync(t)
	var fe *adb.FailError
	_, err := sc.Recv("/sdcard/secret.db", &bytes.Buffer{})
	if !errors.As(err, &fe) || !strings.Contains(fe.Msg, "Permission denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncSend(t *testing.T) {
	sc, srv := openSync(t)
	mtime := time.Unix(1700000500, 0)
	if err := sc.Send("/data/local/tmp/tool", 0o100755, mtime, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	f, ok := srv.Pushed("S1", "/data/local/tmp/tool")
	if !ok || !bytes.Equal(f.Data, big) || f.Mode != 0o100755 || f.MTime.Unix() != mtime.Unix() {
		t.Fatalf("pushed ok=%v mode=%o len=%d", ok, f.Mode, len(f.Data))
	}
}
