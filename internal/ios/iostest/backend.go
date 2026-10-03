// Package iostest provides a fake ios.Backend for tests.
package iostest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

// Backend is a fake usbmuxd/lockdown/AFC/mobilebackup2 stack.
type Backend struct {
	Entries        []ios.Entry
	LockdownValues map[string]any
	LockdownErr    error
	Files          map[string][]byte // AFC paths
	Script         func(*mb2test.Device) error
	ScriptErr      chan error
}

// NewBackend returns an empty fake.
func NewBackend() *Backend {
	return &Backend{Files: map[string][]byte{}, ScriptErr: make(chan error, 1)}
}

func (b *Backend) List(context.Context) ([]ios.Entry, error) { return b.Entries, nil }

func (b *Backend) Values(context.Context, string) (map[string]any, error) {
	return b.LockdownValues, b.LockdownErr
}

func (b *Backend) OpenAFC(context.Context, string) (ios.AFC, error) {
	return fakeAFC{files: b.Files}, nil
}

func (b *Backend) OpenBackup(context.Context, string) (io.ReadWriteCloser, error) {
	if b.Script == nil {
		return nil, errors.New("iostest: no backup script")
	}
	host, dev := mb2test.Pipe()
	go func() {
		err := b.Script(dev)
		_ = dev.Close()
		b.ScriptErr <- err
	}()
	return host, nil
}

type fakeAFC struct{ files map[string][]byte }

func (a fakeAFC) List(dir string) ([]string, error) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]bool{}
	for p := range a.files {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			name, _, _ := strings.Cut(rest, "/")
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return nil, fs.ErrNotExist
	}
	out := []string{".", ".."}
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out[2:])
	return out, nil
}

func (a fakeAFC) Stat(p string) (ios.FileStat, error) {
	if d, ok := a.files[p]; ok {
		return ios.FileStat{Size: int64(len(d))}, nil
	}
	if _, err := a.List(p); err == nil {
		return ios.FileStat{IsDir: true}, nil
	}
	return ios.FileStat{}, fs.ErrNotExist
}

func (a fakeAFC) Open(p string) (io.ReadCloser, error) {
	d, ok := a.files[path.Clean(p)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func (a fakeAFC) Close() error { return nil }
