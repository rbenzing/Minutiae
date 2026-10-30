//go:build scale && windows

package artparse_test

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS of psapi.h.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// peakRSS is the peak working set of this process in bytes, since it started.
func peakRSS() (int64, bool) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)) //nolint:gosec // a fixed-size struct passed to a Windows API
	if r == 0 {
		return 0, false
	}
	return int64(c.peakWorkingSetSize), true //nolint:gosec // a byte count far below MaxInt64
}
