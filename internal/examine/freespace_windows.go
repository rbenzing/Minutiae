//go:build windows

package examine

import (
	"math"

	"golang.org/x/sys/windows"
)

// diskFreeEx is GetDiskFreeSpaceEx: the free bytes available to the caller (quota-aware), the total
// bytes of the volume and the total free bytes (what a quota does not limit).
func diskFreeEx(dir string) (free, total, totalFree uint64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, 0, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, 0, 0, err
	}
	return free, total, totalFree, nil
}

// callerAvailable picks the number the space check must use from the three GetDiskFreeSpaceEx
// results. It is lpFreeBytesAvailableToCaller, the first one: it respects per-user quotas, which the
// total free bytes (the third) do not, so a quota-limited volume is never over-reported (C51).
func callerAvailable(free, _, _ uint64) uint64 {
	return free
}

// diskFree returns the bytes available to the caller on the volume holding dir.
func diskFree(dir string) (int64, error) {
	free, total, totalFree, err := diskFreeEx(dir)
	if err != nil {
		return 0, err
	}
	return availBytes(min(callerAvailable(free, total, totalFree), math.MaxInt64), 1)
}
