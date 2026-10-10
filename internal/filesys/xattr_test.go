package filesys

import (
	"encoding/base64"
	"errors"
	"testing"
)

type xattrStubFS struct{}

func (xattrStubFS) Info() Info                     { return Info{Type: "stub"} }
func (xattrStubFS) Root() Entry                    { return Entry{} }
func (xattrStubFS) ReadDir(Entry) ([]Entry, error) { return nil, nil }
func (xattrStubFS) Lookup(string) (Entry, error)   { return Entry{}, ErrNotFound }
func (xattrStubFS) Open(Entry) (File, error)       { return nil, ErrNotFound }
func (xattrStubFS) Unallocated() ([]Run, error)    { return nil, nil }
func (xattrStubFS) Xattrs(Entry) ([]Xattr, error) {
	return []Xattr{{Name: "a", Size: 1, Storage: XattrInline}}, nil
}

func (xattrStubFS) OpenXattr(Entry, string) (File, error) {
	return nil, ErrNotFound
}

type xattrWrap struct{ inner FileSystem }

func (w xattrWrap) Info() Info                       { return w.inner.Info() }
func (w xattrWrap) Root() Entry                      { return w.inner.Root() }
func (w xattrWrap) ReadDir(e Entry) ([]Entry, error) { return w.inner.ReadDir(e) }
func (w xattrWrap) Lookup(p string) (Entry, error)   { return w.inner.Lookup(p) }
func (w xattrWrap) Open(e Entry) (File, error)       { return w.inner.Open(e) }
func (w xattrWrap) Unallocated() ([]Run, error)      { return w.inner.Unallocated() }
func (w xattrWrap) Underlying() FileSystem           { return w.inner }

func TestXattrReaderFoundThroughWrapper(t *testing.T) {
	var fsys FileSystem = xattrWrap{inner: xattrStubFS{}}
	if _, ok := fsys.(XattrReader); ok {
		t.Fatal("the wrapper itself must not implement XattrReader for this test to mean anything")
	}
	xr, ok := As[XattrReader](fsys)
	if !ok {
		t.Fatal("As[XattrReader] did not find the wrapped reader")
	}
	got, err := xr.Xattrs(Entry{})
	if err != nil || len(got) != 1 || got[0].Name != "a" || got[0].Storage != XattrInline {
		t.Errorf("Xattrs = %+v, %v", got, err)
	}
}

func TestErrXattrCapIsUnsupported(t *testing.T) {
	if !errors.Is(ErrXattrCap, ErrUnsupported) {
		t.Errorf("ErrXattrCap = %v must wrap ErrUnsupported", ErrXattrCap)
	}
	if errors.Is(ErrXattrCap, ErrCorrupt) {
		t.Error("a cap is a bound, not corruption")
	}
}

type plainFS struct{}

func (plainFS) Info() Info                     { return Info{Type: "plain"} }
func (plainFS) Root() Entry                    { return Entry{} }
func (plainFS) ReadDir(Entry) ([]Entry, error) { return nil, nil }
func (plainFS) Lookup(string) (Entry, error)   { return Entry{}, ErrNotFound }
func (plainFS) Open(Entry) (File, error)       { return nil, ErrNotFound }
func (plainFS) Unallocated() ([]Run, error)    { return nil, nil }

func TestXattrReaderNotFoundWhenNoLayerHasIt(t *testing.T) {
	var fsys FileSystem = xattrWrap{inner: xattrWrap{inner: plainFS{}}}
	if xr, ok := As[XattrReader](fsys); ok || xr != nil {
		t.Errorf("As[XattrReader] = %v, %v over a chain with no reader, want nil, false", xr, ok)
	}
}

// The documented display form of a raw attribute name is unpadded base64url.
func TestXattrRawNameVectors(t *testing.T) {
	for _, c := range []struct {
		raw  []byte
		want string
	}{{[]byte{0xff}, "_w"}, {[]byte{0xfb, 0xff, 0xfe}, "-__-"}, {[]byte{0xff, 0xfe}, "__4"}} {
		if got := base64.RawURLEncoding.EncodeToString(c.raw); got != c.want {
			t.Errorf("raw %x = %q, want %q", c.raw, got, c.want)
		}
	}
}
