package examine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func cutDirEnv(t *testing.T) *recEnv {
	t.Helper()
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			if dir.Name == "d" {
				return fmt.Errorf("directory cut: %w", filesys.ErrLimit)
			}
			return nil
		}
	}, delNode("/a.bin", pat(1, bs)), fstest.Node{Path: "/d", Dir: true})
	return e
}

// F81: an id search that met an unreadable directory and did not find the id
// is "search incomplete", never "not found".
func TestRecoverIDSearchWithCutDirectoryIsIncompleteNotNotFound(t *testing.T) {
	e := cutDirEnv(t)
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:no-such-id"}})
	if err == nil {
		t.Fatal("Recover succeeded, want a search-incomplete error")
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("error %v wraps ErrNotFound while a directory could not be read", err)
	}
	var si *examine.SearchIncompleteError
	if !errors.As(err, &si) || !errors.Is(err, filesys.ErrLimit) || si.Unreadable != 1 || !strings.Contains(err.Error(), "/d") {
		t.Errorf("error %v, want *SearchIncompleteError wrapping the cut reason and naming /d", err)
	}
}

// F81: an id that was found is still found although another directory was cut.
func TestRecoverIDSearchFindsIDBesideCutDirectory(t *testing.T) {
	e := cutDirEnv(t)
	sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + idOf2(t, e, "/a.bin")}})
	if err != nil || sum.Recovered != 1 {
		t.Fatalf("Recover = %+v, %v; want the file recovered", sum, err)
	}
}

// F82: Session.Lookup by id never wraps ErrNotFound after an incomplete search.
func TestSessionLookupIDWithCutDirectoryIsIncompleteNotNotFound(t *testing.T) {
	e := cutDirEnv(t)
	fsys, _, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.s.Lookup(fsys, "id:no-such-id")
	var si *examine.SearchIncompleteError
	if err == nil || errors.Is(err, filesys.ErrNotFound) || !errors.As(err, &si) || !errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("Lookup error %v, want *SearchIncompleteError wrapping the cut reason and not ErrNotFound", err)
	}
	if _, _, err := e.s.Lookup(fsys, "id:"+idOf2(t, e, "/a.bin")); err != nil {
		t.Errorf("an id beside the cut directory: %v", err)
	}
}
