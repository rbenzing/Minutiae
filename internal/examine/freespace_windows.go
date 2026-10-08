//go:build windows

package examine

import (
	"math"

	"golang.org/x/sys/windows"
)

// diskFree returns the bytes available to the caller on the volume holding dir.
func diskFree(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return availBytes(min(free, math.MaxInt64), 1)
}
