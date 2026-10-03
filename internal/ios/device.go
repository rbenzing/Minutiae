package ios

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/rbenzing/minutiae/internal/device"
)

// Enumerator lists iOS devices from usbmuxd.
type Enumerator struct{ B Backend }

// Kind is device.KindIOS.
func (Enumerator) Kind() device.Kind { return device.KindIOS }

// List returns every attached device.
func (e Enumerator) List(ctx context.Context) ([]device.Device, error) {
	entries, err := e.B.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]device.Device, 0, len(entries))
	for _, en := range entries {
		out = append(out, &Device{b: e.B, udid: en.UDID})
	}
	return out, nil
}

// Device is one iOS device.
type Device struct {
	b    Backend
	udid string
}

func (d *Device) ID() string        { return d.udid }
func (d *Device) Kind() device.Kind { return device.KindIOS }

// mapErr recognises pairing/trust failures from lockdown.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"pair", "passwordprotected", "invalidhostid", "trust"} {
		if strings.Contains(m, s) {
			return fmt.Errorf("%w: %v", device.ErrUnauthorized, err)
		}
	}
	return err
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool, int, int64, uint64, float64:
		return fmt.Sprint(x)
	}
	return ""
}

// Info reads lockdown values.
func (d *Device) Info(ctx context.Context) (device.Info, error) {
	base := device.Info{ID: d.udid, Kind: device.KindIOS, Manufacturer: "Apple"}
	vals, err := d.b.Values(ctx, d.udid)
	if err != nil {
		return base, mapErr(err)
	}
	base.Model, base.SerialNumber = str(vals["ProductType"]), str(vals["SerialNumber"])
	base.OSVersion = "iOS " + str(vals["ProductVersion"])
	base.Extra = map[string]string{}
	for k, v := range vals {
		if s := str(v); s != "" {
			base.Extra[k] = s
		}
	}
	return base, nil
}

// openAFC opens the AFC service and arranges for it to be closed when ctx is
// cancelled, which is the only way to interrupt a blocked AFC call. The
// returned func closes the client and releases the context hook.
func (d *Device) openAFC(ctx context.Context) (AFC, func(), error) {
	a, err := d.b.OpenAFC(ctx, d.udid)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = a.Close() })
	return a, func() {
		stop()
		_ = a.Close()
	}, nil
}

// ctxErr prefers the context's error once it is done: a cancelled call
// fails with whatever error closing the connection produced.
func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// List lists an AFC (media domain) directory.
func (d *Device) List(ctx context.Context, dir string) ([]device.FileEntry, error) {
	a, done, err := d.openAFC(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	names, err := a.List(dir)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	var out []device.FileEntry
	for _, n := range names {
		if n == "." || n == ".." {
			continue
		}
		p := path.Join(dir, n)
		st, err := a.Stat(p)
		if err != nil {
			return nil, ctxErr(ctx, err)
		}
		mode := fs.FileMode(0o644)
		switch {
		case st.IsDir:
			mode = fs.ModeDir | 0o755
		case st.IsLink:
			mode = fs.ModeSymlink | 0o777
		}
		out = append(out, device.FileEntry{Name: n, Path: p, Size: st.Size, Mode: mode, IsDir: st.IsDir})
	}
	return out, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Pull streams an AFC file into w. The bytes copied must match the size AFC
// reported for the file; a short or long transfer is an error, so the
// artifact is flagged incomplete.
func (d *Device) Pull(ctx context.Context, remotePath string, w io.Writer) (int64, error) {
	a, done, err := d.openAFC(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	st, err := a.Stat(remotePath)
	if err != nil {
		return 0, ctxErr(ctx, err)
	}
	f, err := a.Open(remotePath)
	if err != nil {
		return 0, ctxErr(ctx, err)
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(w, ctxReader{ctx: ctx, r: f})
	if err != nil {
		return n, ctxErr(ctx, err)
	}
	if n != st.Size {
		return n, fmt.Errorf("afc pull %s: copied %d of %d bytes", remotePath, n, st.Size)
	}
	return n, nil
}

// Push is not supported for iOS in Spec 1.
func (d *Device) Push(context.Context, io.Reader, string, fs.FileMode) error {
	return fmt.Errorf("%w: push to iOS", device.ErrUnsupported)
}
