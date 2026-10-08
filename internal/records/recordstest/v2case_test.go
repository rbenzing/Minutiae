package recordstest

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// treeDigest returns "relpath sha256" lines for every file under dir.
func treeDigest(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a test reading a fixture tree
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		sum := sha256.Sum256(b)
		out = append(out, filepath.ToSlash(rel)+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestCopyV2Case(t *testing.T) {
	committed := filepath.Join("testdata", "v2case")
	before := treeDigest(t, committed)
	if len(before) != 5 {
		t.Fatalf("committed fixture holds %d files, want 5: %v", len(before), before)
	}

	dir := CopyV2Case(t)
	if got := treeDigest(t, dir); !equalLines(got, before) {
		t.Fatalf("the copy differs from the committed fixture:\n%v\n%v", got, before)
	}
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.SchemaVersion()
	if err != nil || v != 2 {
		t.Fatalf("schema version = %d, %v; want 2", v, err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.RecordsChecked != 12 || rep.RecordRunsChecked != 2 {
		t.Fatalf("verify: ok=%v records=%d runs=%d problems=%v", rep.OK(), rep.RecordsChecked, rep.RecordRunsChecked, rep.Problems)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	if after := treeDigest(t, committed); !equalLines(after, before) {
		t.Fatalf("the committed fixture changed:\n%v\n%v", after, before)
	}
	if other := CopyV2Case(t); other == dir {
		t.Fatal("two copies share one directory")
	}
}

func equalLines(a, b []string) bool { return slices.Equal(a, b) }
