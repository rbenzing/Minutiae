package ios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	goios "github.com/danielpaulus/go-ios/ios"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/ios/afc"
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

func (GoIOS) Values(_ context.Context, udid string) (vals map[string]any, err error) {
	defer recovered("lockdown values", &err)
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	return goiosValues(d)
}

func (GoIOS) OpenAFC(_ context.Context, udid string) (a AFC, err error) {
	defer recovered("afc start service", &err)
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	rw, err := goiosConnect(d, afcService)
	if err != nil {
		return nil, fmt.Errorf("error connecting to service %q: %w", afcService, err)
	}
	return afcAdapter{c: afc.New(rw)}, nil
}

func (GoIOS) OpenBackup(_ context.Context, udid string) (rw io.ReadWriteCloser, err error) {
	defer recovered("backup start service", &err)
	d, err := entry(udid)
	if err != nil {
		return nil, err
	}
	return goiosConnect(d, "com.apple.mobilebackup2")
}

// afcService is the lockdown service name of the Apple File Conduit.
const afcService = "com.apple.afc"

// afcAdapter exposes the bounds-checked AFC client as the AFC interface. The
// client validates the device's replies itself; the recover guard stays as
// defence in depth against a bug of ours, never as the validation.
type afcAdapter struct{ c *afc.Client }

// maxPanicText bounds the panic value kept in an error message.
const maxPanicText = 200

// recovered turns a panic into *err; use as: defer recovered("afc list", &err).
// The panic value can carry device-influenced text, so it is truncated to
// maxPanicText bytes and quoted (no control bytes reach a log or terminal).
func recovered(op string, err *error) {
	if r := recover(); r != nil {
		s := fmt.Sprint(r)
		if len(s) > maxPanicText {
			s = s[:maxPanicText]
		}
		*err = fmt.Errorf("%s: go-ios panic: %q", op, s)
	}
}

func (a afcAdapter) List(p string) (names []string, err error) {
	defer recovered("afc list", &err)
	return a.c.List(p)
}

func (a afcAdapter) Stat(p string) (st FileStat, err error) {
	defer recovered("afc stat", &err)
	s, err := a.c.Stat(p)
	if err != nil {
		return FileStat{}, err
	}
	out := FileStat{Size: s.Size, IsDir: s.IsDir, IsLink: s.IsLink, Type: s.Type}
	out.Unsupported = !s.IsRegular() && !s.IsDir && !s.IsLink
	return out, nil
}

func (a afcAdapter) Open(p string) (rc io.ReadCloser, err error) {
	defer recovered("afc open", &err)
	f, err := a.c.Open(p)
	if err != nil {
		return nil, err
	}
	return safeFile{f}, nil
}

func (a afcAdapter) Close() (err error) {
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

// The go-ios calls that decode plists the device (or usbmuxd on its behalf)
// controls. They are variables so a test can substitute a shim that panics;
// production code always reaches go-ios through them, under recovered.
var (
	goiosListDevices = rawListDevices
	goiosValues      = func(d goios.DeviceEntry) (map[string]any, error) { return goios.GetValuesPlist(d) }
	goiosConnect     = func(d goios.DeviceEntry, service string) (io.ReadWriteCloser, error) {
		c, err := goios.ConnectToService(d, service)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
)

// listDevices asks usbmuxd for attached devices, one entry per UDID. A panic
// in go-ios while decoding the reply is returned as an error.
func listDevices() (devs []goios.DeviceEntry, err error) {
	defer recovered("usbmuxd list", &err)
	return goiosListDevices()
}

func rawListDevices() ([]goios.DeviceEntry, error) {
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
