// Package ios implements the device backend for iPhone/iPad via usbmuxd.
package ios

import (
	"context"
	"io"
)

// Entry is a device seen by usbmuxd.
type Entry struct {
	UDID           string
	ConnectionType string
}

// FileStat is an AFC stat result.
type FileStat struct {
	Size   int64
	IsDir  bool
	IsLink bool
	// Type is the st_ifmt as the device reported it (empty from a source
	// that does not report one). Unsupported is true when the device gave a
	// type that is none of a regular file, a directory or a link: such an
	// entry is listed as irregular and never pulled.
	Type        string
	Unsupported bool
}

// AFC is the media-domain file service.
type AFC interface {
	List(p string) ([]string, error)
	Stat(p string) (FileStat, error)
	Open(p string) (io.ReadCloser, error)
	Close() error
}

// Backend is everything Minutiae needs from the iOS stack; GoIOS is the real
// one, iostest.Backend the fake.
type Backend interface {
	List(ctx context.Context) ([]Entry, error)
	Values(ctx context.Context, udid string) (map[string]any, error)
	OpenAFC(ctx context.Context, udid string) (AFC, error)
	OpenBackup(ctx context.Context, udid string) (io.ReadWriteCloser, error)
}
