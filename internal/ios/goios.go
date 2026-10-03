package ios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/afc"

	"github.com/rbenzing/minutiae/internal/device"
)

// GoIOS is the real Backend over usbmuxd (Apple Mobile Device Service on
// Windows, installed with iTunes or the Apple Devices app).
type GoIOS struct{}

func (GoIOS) List(context.Context) ([]Entry, error) {
	devs, err := listDevices()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(devs))
	for _, d := range devs {
		out = append(out, Entry{UDID: d.Properties.SerialNumber, ConnectionType: d.Properties.ConnectionType})
	}
	return out, nil
}

func (GoIOS) Values(_ context.Context, udid string) (map[string]any, error) {
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	return goios.GetValuesPlist(d)
}

func (GoIOS) OpenAFC(_ context.Context, udid string) (AFC, error) {
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	c, err := afc.New(d)
	if err != nil {
		return nil, err
	}
	return afcClient{c: c}, nil
}

func (GoIOS) OpenBackup(_ context.Context, udid string) (io.ReadWriteCloser, error) {
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	return goios.ConnectToService(d, "com.apple.mobilebackup2")
}

// afcClient adapts go-ios AFC. go-ios parses device responses without
// guarding every index, so a panic in any call is returned as an error.
type afcClient struct{ c *afc.Client }

// recovered turns a panic into *err; use as: defer recovered("afc list", &err).
func recovered(op string, err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%s: go-ios panic: %v", op, r)
	}
}

func (a afcClient) List(p string) (names []string, err error) {
	defer recovered("afc list", &err)
	return a.c.List(p)
}

func (a afcClient) Stat(p string) (st FileStat, err error) {
	defer recovered("afc stat", &err)
	fi, err := a.c.Stat(p)
	if err != nil {
		return FileStat{}, err
	}
	return FileStat{Size: fi.Size, IsDir: fi.IsDir(), IsLink: fi.IsLink()}, nil
}

func (a afcClient) Open(p string) (rc io.ReadCloser, err error) {
	defer recovered("afc open", &err)
	f, err := a.c.Open(p, afc.READ_ONLY)
	if err != nil {
		return nil, err
	}
	return safeFile{f}, nil
}

func (a afcClient) Close() (err error) {
	defer recovered("afc close", &err)
	return a.c.Close()
}

// safeFile is an open AFC file whose Read and Close never panic.
type safeFile struct{ f io.ReadCloser }

func (s safeFile) Read(p []byte) (n int, err error) {
	defer recovered("afc read", &err)
	return s.f.Read(p)
}

func (s safeFile) Close() (err error) {
	defer recovered("afc close file", &err)
	return s.f.Close()
}

const usbmuxdHint = "usbmuxd not reachable (Windows: install iTunes or the Apple Devices app for Apple Mobile Device Service; Linux: install and start usbmuxd)"

// listDevices asks usbmuxd for attached devices, one entry per UDID.
func listDevices() ([]goios.DeviceEntry, error) {
	conn, err := goios.NewUsbMuxConnectionSimple()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", usbmuxdHint, err)
	}
	defer func() { _ = conn.Close() }()
	dl, err := conn.ListDevices()
	if err != nil {
		return nil, fmt.Errorf("usbmuxd: %w", err)
	}
	return preferUSB(dl.DeviceList), nil
}

// preferUSB keeps one entry per UDID in first-seen order. A device that is
// reachable both over USB and over the network is used through USB.
func preferUSB(in []goios.DeviceEntry) []goios.DeviceEntry {
	at := map[string]int{}
	out := make([]goios.DeviceEntry, 0, len(in))
	for _, d := range in {
		i, seen := at[d.Properties.SerialNumber]
		switch {
		case !seen:
			at[d.Properties.SerialNumber] = len(out)
			out = append(out, d)
		case isUSB(d) && !isUSB(out[i]):
			out[i] = d
		}
	}
	return out
}

func isUSB(d goios.DeviceEntry) bool { return strings.EqualFold(d.Properties.ConnectionType, "USB") }

// entry looks up the usbmuxd entry for udid, preferring USB. Unlike
// goios.GetDevice it never falls back to $udid or the first device.
func entry(udid string) (goios.DeviceEntry, error) {
	if udid == "" {
		return goios.DeviceEntry{}, errors.New("no device UDID given")
	}
	devs, err := listDevices()
	if err != nil {
		return goios.DeviceEntry{}, err
	}
	for _, d := range devs {
		if d.Properties.SerialNumber == udid {
			return d, nil
		}
	}
	return goios.DeviceEntry{}, fmt.Errorf("%w: iOS device %s is not attached", device.ErrNotFound, udid)
}
