package afc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"testing"

	goios "github.com/danielpaulus/go-ios/ios"
	goafc "github.com/danielpaulus/go-ios/ios/afc"

	"github.com/rbenzing/minutiae/internal/ios/afc"
	"github.com/rbenzing/minutiae/internal/ios/afc/afctest"
)

func le(vals ...uint64) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	return b
}

func fakeFiles() *afctest.Server {
	return &afctest.Server{
		Files: map[string][]byte{
			"/DCIM/a.jpg": bytes.Repeat([]byte("jpeg"), 50_000),
			"/DCIM/b.txt": []byte("hi"),
		},
		EmptyDirs: []string{"/Empty"},
	}
}

// The reference reader of the same wire: the go-ios AFC client, run against
// our independently written fake. If framing, opcodes or field layout were
// wrong in the fake, go-ios could not read it.
func TestGoIOSClientReadsOurFakeAFC(t *testing.T) {
	c := goafc.NewFromConn(goios.NewDeviceConnectionWithRWC(afctest.Pipe(fakeFiles())))
	t.Cleanup(func() { _ = c.Close() })
	names, err := c.List("/DCIM")
	if err != nil || len(names) != 2 || names[0] != "a.jpg" || names[1] != "b.txt" {
		t.Fatalf("go-ios List = %v, %v", names, err)
	}
	st, err := c.Stat("/DCIM/a.jpg")
	if err != nil || st.Size != 200_000 || st.IsDir() {
		t.Fatalf("go-ios Stat = %+v, %v", st, err)
	}
	if st, err := c.Stat("/DCIM"); err != nil || !st.IsDir() {
		t.Fatalf("go-ios Stat dir = %+v, %v", st, err)
	}
	f, err := c.Open("/DCIM/a.jpg", goafc.READ_ONLY)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || len(got) != 200_000 || !bytes.Equal(got, bytes.Repeat([]byte("jpeg"), 50_000)) {
		t.Fatalf("go-ios read: %d bytes, %v", len(got), err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat("/nope"); err == nil {
		t.Fatal("go-ios Stat of a missing path succeeded")
	}
}

func TestClientReadsTheSameFake(t *testing.T) {
	srv := fakeFiles()
	c := afc.New(afctest.Pipe(srv))
	t.Cleanup(func() { _ = c.Close() })
	names, err := c.List("/DCIM")
	if err != nil || len(names) != 2 || names[0] != "a.jpg" || names[1] != "b.txt" {
		t.Fatalf("List = %v, %v", names, err)
	}
	if names, err := c.List("/Empty"); err != nil || len(names) != 0 {
		t.Fatalf("an empty directory lists as %v, %v", names, err)
	}
	st, err := c.Stat("/DCIM/a.jpg")
	if err != nil || st.Size != 200_000 || st.IsDir || st.IsLink || st.Type != "S_IFREG" {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	if st, err := c.Stat("/DCIM"); err != nil || !st.IsDir {
		t.Fatalf("Stat dir = %+v, %v", st, err)
	}
	f, err := c.Open("/DCIM/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("jpeg"), 50_000)) {
		t.Fatalf("read: %d bytes, %v", len(got), err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing Stat err = %v", err)
	}
	if _, err := c.Open("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing Open err = %v", err)
	}
}

// The bytes our client puts on the wire, written out from the documented
// layout: "CFA6LPAA", entire length, this length, packet number, operation,
// then the operation's header payload.
func TestClientRequestBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(c *afc.Client)
		want []byte
	}{
		{
			"list", func(c *afc.Client) { _, _ = c.List("/DCIM") },
			append(append([]byte("CFA6LPAA"), le(46, 46, 1, 0x03)...), []byte("/DCIM\x00")...),
		},
		{
			"stat", func(c *afc.Client) { _, _ = c.Stat("/a") },
			append(append([]byte("CFA6LPAA"), le(43, 43, 1, 0x0A)...), []byte("/a\x00")...),
		},
		{
			"open", func(c *afc.Client) { _, _ = c.Open("/a") },
			append(append(append([]byte("CFA6LPAA"), le(51, 51, 1, 0x0D)...), le(1)...), []byte("/a\x00")...),
		},
	} {
		cl, srv := net.Pipe()
		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, len(tc.want))
			_, _ = io.ReadFull(srv, buf)
			got <- buf
			_ = srv.Close()
		}()
		tc.call(afc.New(cl))
		if b := <-got; !bytes.Equal(b, tc.want) {
			t.Errorf("%s: request = % x, want % x", tc.name, b, tc.want)
		}
	}
}

func TestMagicIsCFA6LPAA(t *testing.T) {
	if string(binary.LittleEndian.AppendUint64(nil, afc.Magic)) != "CFA6LPAA" {
		t.Fatalf("magic bytes = %q", binary.LittleEndian.AppendUint64(nil, afc.Magic))
	}
}

func TestClientIsReadOnly(t *testing.T) {
	srv := fakeFiles()
	c := afc.New(afctest.Pipe(srv))
	_, _ = c.List("/DCIM")
	_, _ = c.Stat("/DCIM/b.txt")
	f, err := c.Open("/DCIM/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(f)
	_ = f.Close()
	_ = c.Close()
	allowed := map[uint64]bool{0x03: true, 0x0A: true, 0x0D: true, 0x0F: true, 0x14: true} // read dir, info, open, read, close
	for _, op := range srv.Ops() {
		if !allowed[op] {
			t.Errorf("the client sent operation 0x%02x, which is not a read operation", op)
		}
	}
	// The fake refuses any open mode other than 1 (read only), so a
	// successful Open above also proves the mode on the wire.
}
