// Package device defines the backend-neutral device abstraction and the
// audited acquisition helpers every backend uses.
package device

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Kind identifies a backend.
type Kind string

const (
	KindAndroid Kind = "android"
	KindIOS     Kind = "ios"
	KindSerial  Kind = "serial"
)

var (
	ErrNotFound              = errors.New("device not found")
	ErrUnauthorized          = errors.New("device not authorized (unlock it and accept the trust/debugging prompt)")
	ErrNotRooted             = errors.New("device is not rooted")
	ErrUnsupported           = errors.New("operation not supported by this device")
	ErrDeviceWriteNotAllowed = errors.New("writing to the device requires --allow-device-write")
)

// Info describes a device.
type Info struct {
	ID           string            `json:"id"`
	Kind         Kind              `json:"kind"`
	Model        string            `json:"model,omitempty"`
	Manufacturer string            `json:"manufacturer,omitempty"`
	OSVersion    string            `json:"os_version,omitempty"`
	SerialNumber string            `json:"serial_number,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// FileEntry is one directory entry on a device.
type FileEntry struct {
	Name    string      `json:"name"`
	Path    string      `json:"path"`
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"mod_time"`
	IsDir   bool        `json:"is_dir"`
}

// Partition is one block device that can be imaged.
type Partition struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ProgressFunc reports bytes done of total (total < 0 when unknown).
type ProgressFunc func(done, total int64)

// LogicalOptions configure a logical acquisition.
type LogicalOptions struct {
	Roots []string
}

type Device interface {
	ID() string
	Kind() Kind
	Info(ctx context.Context) (Info, error)
}

type FileTransferer interface {
	List(ctx context.Context, path string) ([]FileEntry, error)
	Pull(ctx context.Context, remotePath string, w io.Writer) (int64, error)
	Push(ctx context.Context, r io.Reader, remotePath string, mode fs.FileMode) error
}

type Imager interface {
	Partitions(ctx context.Context) ([]Partition, error)
	Image(ctx context.Context, partition string, w io.Writer, progress ProgressFunc) (int64, error)
}

type LogicalAcquirer interface {
	AcquireLogical(ctx context.Context, c *evidence.Case, opts LogicalOptions, progress ProgressFunc) error
}

type Enumerator interface {
	Kind() Kind
	List(ctx context.Context) ([]Device, error)
}

type progressWriter struct {
	w        io.Writer
	done     int64
	total    int64
	progress ProgressFunc
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.progress != nil {
		p.progress(p.done, p.total)
	}
	return n, err
}

// WriteCounter wraps w so that every write reports progress.
func WriteCounter(w io.Writer, total int64, progress ProgressFunc) io.Writer {
	return &progressWriter{w: w, total: total, progress: progress}
}
