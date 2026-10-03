package ios

import "io/fs"

// SetWalkDir replaces the staging walker for a test; the returned func restores it.
func SetWalkDir(f func(string, fs.WalkDirFunc) error) (restore func()) {
	orig := walkDir
	walkDir = f
	return func() { walkDir = orig }
}
