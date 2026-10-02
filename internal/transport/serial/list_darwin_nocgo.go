//go:build darwin && !cgo

package serial

import (
	"fmt"

	bug "go.bug.st/serial"
)

// List enumerates port names only. USB details on macOS need IOKit via cgo,
// and Minutiae is built without cgo (spec §2), so IsUSB is false and the
// VID/PID/serial/product fields are empty.
func (System) List() ([]PortInfo, error) {
	names, err := bug.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n})
	}
	return out, nil
}
