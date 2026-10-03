package ios

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"howett.net/plist"
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
