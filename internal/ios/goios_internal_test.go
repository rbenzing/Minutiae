package ios

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/ios/afc/afctest"
)

// fakeUsbmuxd answers every ListDevices request with devices and points
// go-ios at itself through USBMUXD_SOCKET_ADDRESS.
func fakeUsbmuxd(t *testing.T, devices ...map[string]any) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	payload, err := plist.Marshal(map[string]any{"DeviceList": devices}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var hdr [16]byte
				if _, err := io.ReadFull(conn, hdr[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(binary.LittleEndian.Uint32(hdr[:4]))-16); err != nil {
					return
				}
				var out bytes.Buffer
				for _, v := range []uint32{uint32(16 + len(payload)), 1, 8, binary.LittleEndian.Uint32(hdr[12:])} {
					_ = binary.Write(&out, binary.LittleEndian, v)
				}
				out.Write(payload)
				_, _ = conn.Write(out.Bytes())
			}()
		}
	}()
	t.Setenv("USBMUXD_SOCKET_ADDRESS", ln.Addr().String())
}

func muxDevice(id int, udid, conn string) map[string]any {
	return map[string]any{"MessageType": "Attached", "DeviceID": id, "Properties": map[string]any{
		"DeviceID": id, "SerialNumber": udid, "ConnectionType": conn,
	}}
}

func dualHomedU1(t *testing.T) {
	t.Helper()
	fakeUsbmuxd(t, muxDevice(7, "U1", "Network"), muxDevice(3, "U1", "USB"), muxDevice(9, "U2", "Network"), muxDevice(4, "U1", "Network"))
}

func TestGoIOSListPrefersUSBOverNetwork(t *testing.T) {
	dualHomedU1(t)
	got, err := GoIOS{}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{UDID: "U1", ConnectionType: "USB"}, {UDID: "U2", ConnectionType: "Network"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
}

func TestGoIOSEntryPrefersUSBOverNetwork(t *testing.T) {
	dualHomedU1(t)
	e, err := entry("U1")
	if err != nil || e.DeviceID != 3 {
		t.Fatalf("entry(U1) = device %d, %v; want the USB entry (device 3)", e.DeviceID, err)
	}
	if e, err := entry("U2"); err != nil || e.DeviceID != 9 {
		t.Fatalf("entry(U2) = device %d, %v", e.DeviceID, err)
	}
	if _, err := entry(""); err == nil {
		t.Fatal("an empty UDID must not select an arbitrary device")
	}
}

func TestGoIOSUnreachableUsbmuxdHint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now
	t.Setenv("USBMUXD_SOCKET_ADDRESS", addr)
	const hint = "usbmuxd not reachable (Windows: install iTunes or the Apple Devices app for Apple Mobile Device Service; Linux: install and start usbmuxd)"
	if _, err := (GoIOS{}).List(context.Background()); err == nil || !strings.Contains(err.Error(), hint) {
		t.Fatalf("List err = %v", err)
	}
	if _, err := (GoIOS{}).Values(context.Background(), "U1"); err == nil || !strings.Contains(err.Error(), hint) {
		t.Fatalf("Values err = %v", err)
	}
}

func TestGoIOSOpenAFCUsesTheBoundsCheckedClient(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	srv := &afctest.Server{Files: map[string][]byte{"/DCIM/a.txt": []byte("hello")}}
	var service string
	goiosConnect = func(_ goios.DeviceEntry, svc string) (io.ReadWriteCloser, error) {
		service = svc
		return afctest.Pipe(srv), nil
	}
	a, err := (GoIOS{}).OpenAFC(context.Background(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if service != "com.apple.afc" {
		t.Fatalf("service = %q", service)
	}
	names, err := a.List("/DCIM")
	if err != nil || len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("List = %v, %v", names, err)
	}
	st, err := a.Stat("/DCIM/a.txt")
	if err != nil || st.Size != 5 || st.IsDir {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	f, err := a.Open("/DCIM/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, f); err != nil || buf.String() != "hello" {
		t.Fatalf("read = %q, %v", buf.String(), err)
	}
	_ = f.Close()
}

// goiosDevice returns the iOS device U1 reached through the real GoIOS
// backend, whose service connection is the given fake AFC server.
func goiosDevice(t *testing.T, srv *afctest.Server) *Device {
	t.Helper()
	dualHomedU1(t)
	restoreSeams(t)
	goiosConnect = func(goios.DeviceEntry, string) (io.ReadWriteCloser, error) { return afctest.Pipe(srv), nil }
	devs, err := Enumerator{B: GoIOS{}}.List(context.Background())
	if err != nil || len(devs) == 0 {
		t.Fatalf("List = %v, %v", devs, err)
	}
	return devs[0].(*Device)
}

// W53: a silent device (it accepts the request and never answers) must not
// hang a pull or a listing past the deadline: cancelling the context closes
// the real AFC client's connection, which unblocks the read.
func TestSilentDeviceDoesNotHangPullOrList(t *testing.T) {
	silent := func() *afctest.Server {
		return &afctest.Server{Hook: func(uint64, []byte) ([]byte, bool) { return nil, true }}
	}
	for name, call := range map[string]func(context.Context, *Device) error{
		"Pull": func(ctx context.Context, d *Device) error { _, err := d.Pull(ctx, "/x", io.Discard); return err },
		"List": func(ctx context.Context, d *Device) error { _, err := d.List(ctx, "/"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			d := goiosDevice(t, silent())
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx, d) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want the deadline", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked on a silent device despite the cancelled context")
			}
		})
	}
}

// statServer answers every file-info with the given st_ifmt and a 5-byte size.
func statServer(ifmt string) *afctest.Server {
	n := uint64(0)
	return &afctest.Server{
		Files: map[string][]byte{"/x": []byte("hello")},
		Hook: func(op uint64, _ []byte) ([]byte, bool) {
			n++
			if op != 0x0A {
				return nil, false
			}
			return afctest.Frame(2, n, nil, []byte("st_size\x005\x00st_ifmt\x00"+ifmt+"\x00")), true
		},
	}
}

// W51: an entry whose st_ifmt is none of regular, directory or link is
// reported as unsupported and is never read as a regular file.
func TestPullRefusesAnUnknownFileTypeBeforeOpening(t *testing.T) {
	srv := statServer("S_IFSOCK")
	d := goiosDevice(t, srv)
	var buf bytes.Buffer
	n, err := d.Pull(context.Background(), "/x", &buf)
	if !errors.Is(err, device.ErrUnsupported) || !strings.Contains(err.Error(), `"S_IFSOCK"`) {
		t.Fatalf("err = %v, want device.ErrUnsupported naming the type", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Fatalf("%d bytes copied from an unsupported entry", buf.Len())
	}
	for _, op := range srv.Ops() {
		if op == 0x0D || op == 0x0F {
			t.Fatalf("operation 0x%02x was sent for an unsupported entry", op)
		}
	}
}

func TestStatReportsAnUnknownFileTypeAsUnsupported(t *testing.T) {
	for ifmt, want := range map[string]bool{"S_IFREG": false, "S_IFDIR": false, "S_IFLNK": false, "S_IFSOCK": true, "weird": true, "": true} {
		d := goiosDevice(t, statServer(ifmt))
		a, err := d.b.OpenAFC(context.Background(), "U1")
		if err != nil {
			t.Fatal(err)
		}
		st, err := a.Stat("/x")
		_ = a.Close()
		if err != nil || st.Unsupported != want || st.Type != ifmt {
			t.Errorf("%q: Stat = %+v, %v; want Unsupported %v", ifmt, st, err, want)
		}
	}
}

// W60: over the real client a directory and a link are never opened.
func TestPullOfADirectoryOrLinkSendsNoOpen(t *testing.T) {
	for _, ifmt := range []string{"S_IFDIR", "S_IFLNK"} {
		srv := statServer(ifmt)
		d := goiosDevice(t, srv)
		if _, err := d.Pull(context.Background(), "/x", io.Discard); !errors.Is(err, device.ErrUnsupported) || !strings.Contains(err.Error(), ifmt) {
			t.Fatalf("%s: err = %v", ifmt, err)
		}
		for _, op := range srv.Ops() {
			if op == 0x0D || op == 0x0F {
				t.Fatalf("%s: operation 0x%02x was sent", ifmt, op)
			}
		}
	}
}

// trackedConn records Close.
type trackedConn struct {
	io.ReadWriteCloser
	closed chan struct{}
}

func (c trackedConn) Close() error { close(c.closed); return nil }

// W59: the service connection (lockdown + StartService, which go-ios cannot
// cancel) runs in a guarded goroutine; a cancelled context returns at once and
// a connection that arrives late is closed, never leaked.
func TestOpenAFCReturnsOnCancelAndClosesALateConnection(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	conn := trackedConn{ReadWriteCloser: afctest.Pipe(&afctest.Server{}), closed: make(chan struct{})}
	goiosConnect = func(goios.DeviceEntry, string) (io.ReadWriteCloser, error) {
		close(entered)
		<-release
		return conn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := (GoIOS{}).OpenAFC(ctx, "U1"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenAFC stayed blocked in the connect after the context was cancelled")
	}
	close(release)
	select {
	case <-conn.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the late connection was not closed")
	}
}

// A panic in the connect goroutine is still returned as an error.
func TestOpenAFCPanicInTheConnectGoroutineIsAnError(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	goiosConnect = func(goios.DeviceEntry, string) (io.ReadWriteCloser, error) { panic("hostile StartService plist") }
	a, err := (GoIOS{}).OpenAFC(context.Background(), "U1")
	if a != nil {
		t.Fatalf("OpenAFC returned %v", a)
	}
	wantRecoveredPanic(t, "OpenAFC", err)
}
