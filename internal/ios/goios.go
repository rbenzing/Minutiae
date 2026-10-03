package ios

import (
	"context"
	"fmt"
	"io"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/afc"
)

// GoIOS is the real Backend over usbmuxd (Apple Mobile Device Service on
// Windows, installed with iTunes or the Apple Devices app).
type GoIOS struct{}

func (GoIOS) List(context.Context) ([]Entry, error) {
	dl, err := goios.ListDevices()
	if err != nil {
		return nil, fmt.Errorf("usbmuxd: %w", err)
	}
	out := make([]Entry, 0, len(dl.DeviceList))
	for _, d := range dl.DeviceList {
		out = append(out, Entry{UDID: d.Properties.SerialNumber, ConnectionType: d.Properties.ConnectionType})
	}
	return out, nil
}

func (GoIOS) Values(_ context.Context, udid string) (map[string]any, error) {
	d, err := goios.GetDevice(udid)
	if err != nil {
		return nil, err
	}
	return goios.GetValuesPlist(d)
}

func (GoIOS) OpenAFC(_ context.Context, udid string) (AFC, error) {
	d, err := goios.GetDevice(udid)
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
	d, err := goios.GetDevice(udid)
	if err != nil {
		return nil, err
	}
	return goios.ConnectToService(d, "com.apple.mobilebackup2")
}

type afcClient struct{ c *afc.Client }

func (a afcClient) List(p string) ([]string, error) { return a.c.List(p) }

func (a afcClient) Stat(p string) (FileStat, error) {
	fi, err := a.c.Stat(p)
	if err != nil {
		return FileStat{}, err
	}
	return FileStat{Size: fi.Size, IsDir: fi.IsDir(), IsLink: fi.IsLink()}, nil
}

func (a afcClient) Open(p string) (io.ReadCloser, error) { return a.c.Open(p, afc.READ_ONLY) }

func (a afcClient) Close() error { return a.c.Close() }
