//go:build scale && linux

package artparse_test

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// peakRSS is VmHWM of /proc/self/status (the peak resident set) in bytes.
func peakRSS() (int64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "VmHWM:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}
