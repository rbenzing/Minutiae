// Package android implements the device backend for Android over ADB.
package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/device"
)

// Enumerator lists devices from the ADB server.
type Enumerator struct{ C *adb.Client }

// Kind is device.KindAndroid.
func (Enumerator) Kind() device.Kind { return device.KindAndroid }

// List returns every device the server knows, whatever its state.
func (e Enumerator) List(ctx context.Context) ([]device.Device, error) {
	entries, err := e.C.Devices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]device.Device, 0, len(entries))
	for _, en := range entries {
		out = append(out, &Device{c: e.C, serial: en.Serial, state: en.State, props: en.Props})
	}
	return out, nil
}

// Device is one Android device.
type Device struct {
	c      *adb.Client
	serial string
	state  string
	props  map[string]string
	su     func(string) string //nolint:unused // set once root is detected (root detection, Task 5)
}

func (d *Device) ID() string        { return d.serial }
func (d *Device) Kind() device.Kind { return device.KindAndroid }

func (d *Device) ready() error {
	switch d.state {
	case "device":
		return nil
	case "unauthorized":
		return fmt.Errorf("%s: %w", d.serial, device.ErrUnauthorized)
	default:
		return fmt.Errorf("%s is %q, not ready", d.serial, d.state)
	}
}

// mapErr turns ADB FAIL messages into device sentinel errors.
func mapErr(err error) error {
	var fe *adb.FailError
	if !errors.As(err, &fe) {
		return err
	}
	m := strings.ToLower(fe.Msg)
	switch {
	case strings.Contains(m, "unauthorized"):
		return fmt.Errorf("%w: %s", device.ErrUnauthorized, fe.Msg)
	case strings.Contains(m, "device") && strings.Contains(m, "not found"):
		return fmt.Errorf("%w: %s", device.ErrNotFound, fe.Msg)
	}
	return err
}

var getpropLine = regexp.MustCompile(`^\[(.+?)\]: \[(.*)\]$`)

// ParseGetprop parses `getprop` output ("[key]: [value]" per line).
func ParseGetprop(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if m := getpropLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// Info reads system properties.
func (d *Device) Info(ctx context.Context) (device.Info, error) {
	base := device.Info{ID: d.serial, Kind: device.KindAndroid}
	if err := d.ready(); err != nil {
		return base, err
	}
	out, err := d.c.Output(ctx, d.serial, "getprop")
	if err != nil {
		return base, mapErr(err)
	}
	p := ParseGetprop(out)
	base.Model, base.Manufacturer = p["ro.product.model"], p["ro.product.manufacturer"]
	base.OSVersion, base.SerialNumber = "Android "+p["ro.build.version.release"], p["ro.serialno"]
	base.Extra = p
	return base, nil
}

func (d *Device) sync(ctx context.Context) (*adb.Sync, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	s, err := d.c.Sync(ctx, d.serial)
	return s, mapErr(err)
}

func toFileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o100000:
	default:
		mode |= fs.ModeIrregular
	}
	return mode
}

// List lists a remote directory.
func (d *Device) List(ctx context.Context, dir string) ([]device.FileEntry, error) {
	s, err := d.sync(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	es, err := s.List(dir)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]device.FileEntry, 0, len(es))
	for _, e := range es {
		out = append(out, device.FileEntry{
			Name: e.Name, Path: path.Join(dir, e.Name), Size: int64(e.Size),
			Mode: toFileMode(e.Mode), ModTime: e.MTime, IsDir: e.IsDir(),
		})
	}
	return out, nil
}

// Pull streams a remote file into w.
func (d *Device) Pull(ctx context.Context, remotePath string, w io.Writer) (int64, error) {
	s, err := d.sync(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	n, err := s.Recv(remotePath, w)
	return n, mapErr(err)
}

// Push writes r to remotePath. Callers must go through device.PushAudited.
func (d *Device) Push(ctx context.Context, r io.Reader, remotePath string, mode fs.FileMode) error {
	s, err := d.sync(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return mapErr(s.Send(remotePath, 0o100000|uint32(mode.Perm()), time.Now(), r))
}
