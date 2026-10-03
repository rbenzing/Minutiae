package detect_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func mtfsDriver() detect.Driver {
	return detect.Driver{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open}
}

func image() []byte {
	return fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/a", Data: []byte("x")}}})
}

func TestOpenWithNoMatch(t *testing.T) {
	img := bytes.Repeat([]byte{0x55}, 4096)
	for name, drivers := range map[string][]detect.Driver{
		"nil":         nil,
		"empty":       {},
		"no match":    {mtfsDriver()},
		"nil funcs":   {{Name: "broken"}},
		"nil probe":   {{Name: "broken", Open: fstest.Open}},
		"nil open":    {{Name: "broken", Probe: func(io.ReaderAt, int64) bool { return true }}},
		"not matched": {{Name: "never", Probe: func(io.ReaderAt, int64) bool { return false }, Open: fstest.Open}},
	} {
		t.Run(name, func(t *testing.T) {
			fsys, err := detect.OpenWith(drivers, bytes.NewReader(img), int64(len(img)))
			if fsys != nil || !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "no recognized filesystem") {
				t.Errorf("OpenWith = %v, %v; want ErrUnsupported (no recognized filesystem)", fsys, err)
			}
		})
	}
	// The package-level registry starts empty.
	if len(detect.Drivers) != 0 {
		t.Errorf("Drivers = %d entries, want none until the filesystem packages land", len(detect.Drivers))
	}
	if _, err := detect.Open(bytes.NewReader(img), int64(len(img))); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open err = %v, want ErrUnsupported", err)
	}
	if name, ok := detect.Probe(bytes.NewReader(img), int64(len(img))); ok {
		t.Errorf("Probe = %q, true; want no match", name)
	}
}

func TestOpenWithFirstMatchWins(t *testing.T) {
	img := image()
	var opened []string
	mk := func(name string, match bool) detect.Driver {
		return detect.Driver{
			Name:  name,
			Probe: func(io.ReaderAt, int64) bool { return match },
			Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
				opened = append(opened, name)
				return fstest.Open(r, size)
			},
		}
	}
	drivers := []detect.Driver{mk("first", false), mk("second", true), mk("third", true)}
	fsys, err := detect.OpenWith(drivers, bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if fsys.Info().Type != "mtfs" {
		t.Errorf("Info = %+v", fsys.Info())
	}
	if strings.Join(opened, ",") != "second" {
		t.Errorf("opened = %v, want only second", opened)
	}

	// A matching driver whose Open fails is final: later drivers are not tried.
	boom := errors.New("boom")
	failing := detect.Driver{
		Name:  "failing",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return nil, boom },
	}
	if _, err := detect.OpenWith([]detect.Driver{failing, mtfsDriver()}, bytes.NewReader(img), int64(len(img))); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}

	if name, ok := detect.ProbeWith(drivers, bytes.NewReader(img), int64(len(img))); !ok || name != "second" {
		t.Errorf("ProbeWith = %q, %v", name, ok)
	}
}

func TestDetectRecoversPanic(t *testing.T) {
	img := image()
	panicOpen := detect.Driver{
		Name:  "evil",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) {
			i := len(img)
			_ = img[i] // index out of range
			return nil, nil
		},
	}
	fsys, err := detect.OpenWith([]detect.Driver{panicOpen}, bytes.NewReader(img), int64(len(img)))
	var ce *filesys.CorruptError
	if fsys != nil || !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("OpenWith = %v, %v; want *CorruptError", fsys, err)
	}
	if ce.Structure != "evil" || !strings.HasPrefix(ce.Reason, "parser panic: ") || !strings.Contains(ce.Reason, "index out of range") {
		t.Errorf("CorruptError = %+v", ce)
	}

	// A panic in Probe is a non-match, not a crash.
	panicProbe := detect.Driver{
		Name:  "evilprobe",
		Probe: func(io.ReaderAt, int64) bool { panic("probe boom") },
		Open:  fstest.Open,
	}
	if _, err := detect.OpenWith([]detect.Driver{panicProbe, mtfsDriver()}, bytes.NewReader(img), int64(len(img))); err != nil {
		t.Errorf("a panicking Probe must fall through to the next driver: %v", err)
	}

	// (nil, nil) from a driver is reported, not returned as a nil filesystem.
	nilFS := detect.Driver{
		Name: "nilfs", Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) { return nil, nil },
	}
	if fsys, err := detect.OpenWith([]detect.Driver{nilFS}, bytes.NewReader(img), int64(len(img))); fsys != nil || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("OpenWith(nil,nil driver) = %v, %v", fsys, err)
	}
}
