package recordstest

import (
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// v2caseFS is the committed schema v2 case, produced by the 5A code (commit
// 5ad6b0e) before any schema v3 change: one 256-byte artifact and 12 records in
// two complete ingests of parser fixparser (6 each; 2.0 supersedes 1.0). It is
// history: never regenerate or edit it (.gitattributes keeps its bytes exact so
// the audit hash chain survives).
//
//go:embed all:testdata/v2case
var v2caseFS embed.FS

// CopyV2Case copies the committed schema v2 case (12 records in 2 runs of 6; the
// second supersedes the first) into t.TempDir() and returns its directory. The
// case is closed: open it with evidence.Open. Callers may modify the copy freely.
func CopyV2Case(t testing.TB) string {
	t.Helper()
	root := "testdata/v2case"
	dst := filepath.Join(t.TempDir(), "V2FIX")
	err := fs.WalkDir(v2caseFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := p[len(root):]
		target := filepath.Join(dst, filepath.FromSlash(path.Clean("/" + rel)[1:]))
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := v2caseFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
