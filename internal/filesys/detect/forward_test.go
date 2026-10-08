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

// The first driver matches and fails with a corrupt-structure error, a later one opens: the result is
// the wrapper that carries the note, and the optional interfaces of the reader underneath (here the
// Recoverer) are still found through filesys.As and the wrapper's Underlying.
func TestWarnedForwardsRecoverer(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/old", Data: []byte("x"), Deleted: true, Recover: []fstest.RecoverMap{{Method: "own-runs"}}},
	}})
	corrupt := detect.Driver{
		Name:  "first",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) {
			return nil, &filesys.CorruptError{Structure: "first", Offset: 1, Reason: "bad first"}
		},
	}
	fsys, err := detect.OpenWith([]detect.Driver{corrupt, mtfsDriver()}, bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if w := fsys.Info().Warnings; len(w) == 0 || !strings.Contains(w[0], "first matched but failed to open") {
		t.Fatalf("Info().Warnings = %q, want the note first (the test must exercise the wrapper)", w)
	}
	wr, ok := fsys.(filesys.Wrapper)
	if !ok {
		t.Fatalf("%T is not a filesys.Wrapper", fsys)
	}
	if u := wr.Underlying(); u == nil || u.Info().Type != "mtfs" || len(u.Info().Warnings) != 0 {
		t.Errorf("Underlying() = %v, want the bare mtfs reader", u)
	}
	rec, ok := filesys.As[filesys.Recoverer](fsys)
	if !ok {
		t.Fatal("filesys.As does not find the Recoverer behind the wrapper")
	}
	es, err := fsys.ReadDir(fsys.Root())
	if err != nil || len(es) != 1 {
		t.Fatalf("ReadDir = %v, %v", es, err)
	}
	cs, err := rec.Recoverable(es[0])
	if err != nil || len(cs) != 1 || cs[0].Method != "own-runs" {
		t.Errorf("Recoverable through the wrapper = %v, %v", cs, err)
	}
	if _, err := rec.Recoverable(filesys.Entry{ID: "nope"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown id = %v, want ErrNotFound", err)
	}
	// and the wrapper invents nothing: a reader without a Journaler gives none
	if _, ok := filesys.As[filesys.Journaler](fsys); ok {
		t.Error("the wrapper offers a Journaler the reader does not have")
	}
}
