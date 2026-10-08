//go:build !windows && !linux

package examine

import "golang.org/x/sys/unix"

// diskFree returns the bytes available to the caller on the filesystem holding dir.
func diskFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return availBytes(st.Bavail, int64(st.Bsize)) // Bsize is int64 on linux and uint32 on darwin
}
