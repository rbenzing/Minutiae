//go:build !darwin || cgo

package serial

import (
	"fmt"

	"go.bug.st/serial/enumerator"
)

// List enumerates ports with USB details when available.
func (System) List() ([]PortInfo, error) {
	ds, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]PortInfo, 0, len(ds))
	for _, d := range ds {
		out = append(out, PortInfo{Name: d.Name, IsUSB: d.IsUSB, VID: d.VID, PID: d.PID, SerialNumber: d.SerialNumber, Product: d.Product})
	}
	return out, nil
}
