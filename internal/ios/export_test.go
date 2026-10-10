package ios

import (
	"context"
	"io"
	"io/fs"

	"github.com/rbenzing/minutiae/internal/ios/manifestdb"
)

// SetWalkDir replaces the staging walker for a test; the returned func restores it.
func SetWalkDir(f func(string, fs.WalkDirFunc) error) (restore func()) {
	orig := walkDir
	walkDir = f
	return func() { walkDir = orig }
}

// SetReadManifest replaces the Manifest.db reader for a test; the returned func restores it.
func SetReadManifest(f func(context.Context, io.ReaderAt, int64, func(string) bool) (*manifestdb.Declared, error)) (restore func()) {
	orig := readManifest
	readManifest = f
	return func() { readManifest = orig }
}

// SetFoldKey replaces the path-folding function (mb2.FoldKey) for a test.
func SetFoldKey(f func(string) string) (restore func()) {
	orig := foldKey
	foldKey = f
	return func() { foldKey = orig }
}
