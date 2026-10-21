//go:build !windows

package main

// finalDir is the identity off Windows: filepath.EvalSymlinks already resolved every link.
func finalDir(p string) (string, error) { return p, nil }
