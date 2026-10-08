//go:build linux

package examine

import "golang.org/x/sys/unix"

// diskFree returns the bytes available to the caller on the filesystem holding dir. On Linux f_bavail
// counts f_frsize units.
func diskFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return availBytes(st.Bavail, statfsUnit(int64(st.Bsize), int64(st.Frsize)))
}
