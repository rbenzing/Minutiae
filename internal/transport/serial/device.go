package serial

import (
	"context"
	"strconv"

	"github.com/rbenzing/minutiae/internal/device"
)

// Enumerator exposes serial ports to the device registry.
type Enumerator struct{ P Provider }

// Kind is device.KindSerial.
func (Enumerator) Kind() device.Kind { return device.KindSerial }

// List returns one Device per port.
func (e Enumerator) List(context.Context) ([]device.Device, error) {
	ports, err := e.P.List()
	if err != nil {
		return nil, err
	}
	out := make([]device.Device, 0, len(ports))
	for _, p := range ports {
		out = append(out, Device{Port: p})
	}
	return out, nil
}

// Device is a serial port seen as a device.
type Device struct{ Port PortInfo }

func (d Device) ID() string        { return d.Port.Name }
func (d Device) Kind() device.Kind { return device.KindSerial }

// Info reports USB identity; it never touches the port.
func (d Device) Info(context.Context) (device.Info, error) {
	return device.Info{
		ID: d.Port.Name, Kind: device.KindSerial, Model: d.Port.Product, SerialNumber: d.Port.SerialNumber,
		Extra: map[string]string{"vid": d.Port.VID, "pid": d.Port.PID, "usb": strconv.FormatBool(d.Port.IsUSB)},
	}, nil
}
