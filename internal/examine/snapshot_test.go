package examine_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// snapFS adds filesys.Snapshotter to an MTFS filesystem.
type snapFS struct {
	filesys.FileSystem
	fn func(p, snapshot string) (string, error)
}

func (s snapFS) SnapshotPath(p, snapshot string) (string, error) { return s.fn(p, snapshot) }

func snapSession(t *testing.T, fn func(p, snapshot string) (string, error)) *examine.Session {
	t.Helper()
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 0, Nodes: []fstest.Node{{Path: "/a", Data: []byte("a")}}}))
	recs := importImage(t, c, data, 1)
	opts := examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			if fn == nil {
				return fsys, nil
			}
			return snapFS{FileSystem: fsys, fn: fn}, nil
		},
	}}}
	s, err := examine.Open(c, recs[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionSnapshotPathForwardsThroughSafeFS(t *testing.T) {
	s := snapSession(t, func(p, snapshot string) (string, error) {
		switch snapshot {
		case "panic":
			panic("boom")
		case "missing":
			return "", errors.Join(filesys.ErrNotFound, errors.New("no such snapshot"))
		}
		return "/V/.snapshots/" + snapshot + p, nil
	})
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SnapshotPath(fsys, "S", "/x/y"); err != nil || got != "/V/.snapshots/S/x/y" {
		t.Errorf("SnapshotPath = %q, %v", got, err)
	}
	if _, err := s.SnapshotPath(fsys, "missing", "/x"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an error of the filesystem must pass through: %v", err)
	}
	// A panic in the parser becomes a CorruptError, not a crash.
	if _, err := s.SnapshotPath(fsys, "panic", "/x"); !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "SnapshotPath") {
		t.Errorf("a panicking SnapshotPath = %v, want a CorruptError", err)
	}
}

func TestSessionSnapshotPathUnsupportedFS(t *testing.T) {
	s := snapSession(t, nil)
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SnapshotPath(fsys, "S", "/a")
	if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "filesystem has no snapshots") {
		t.Errorf("SnapshotPath on a filesystem without snapshots = %v, want ErrUnsupported (filesystem has no snapshots)", err)
	}
}

// Recursive extraction skips the synthetic .snapshots directory unless the
// examiner addresses it; a snapshot path given explicitly is extracted.
func TestExtractRecursiveSkipsSnapshotsUnlessAddressed(t *testing.T) {
	hook := hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
		if e.Name == ".snapshots" && e.Type == filesys.TypeDir {
			e.Attrs = append(e.Attrs, filesys.KV{Key: "synthetic", Value: "snapshots"})
		}
		return e
	}}
	nodes := []fstest.Node{
		{Path: "/live.txt", Data: []byte("live")},
		{Path: "/.snapshots/S/old.txt", Data: []byte("old")},
	}
	paths := func(sum examine.Summary) []string {
		var out []string
		for _, a := range sum.Artifacts {
			out = append(out, a.Source.RemotePath)
		}
		return out
	}

	s, _ := sessionHook(t, newCase(t), hook, 0, nodes...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if got := paths(sum); len(got) != 1 || got[0] != "/live.txt" || sum.Skipped != 0 {
		t.Errorf("extract -r / = %v (skipped %d), want only /live.txt", got, sum.Skipped)
	}

	s, _ = sessionHook(t, newCase(t), hook, 0, nodes...)
	sum = extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/.snapshots"}, Recursive: true})
	if got := paths(sum); len(got) != 1 || got[0] != "/.snapshots/S/old.txt" {
		t.Errorf("extract -r /.snapshots = %v, want the snapshot file", got)
	}
	s, _ = sessionHook(t, newCase(t), hook, 0, nodes...)
	sum = extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/.snapshots/S/old.txt"}})
	if got := paths(sum); len(got) != 1 || got[0] != "/.snapshots/S/old.txt" {
		t.Errorf("extract of a snapshot file = %v", got)
	}
}
