package serial

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
)

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	bad := []func(*Config){
		func(c *Config) { c.Baud = 0 },
		func(c *Config) { c.DataBits = 9 },
		func(c *Config) { c.Parity = "foo" },
		func(c *Config) { c.StopBits = "3" },
	}
	for i, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("case %d: invalid config accepted: %+v", i, c)
		}
	}
}

type fakeProvider struct{ ports []PortInfo }

func (f fakeProvider) List() ([]PortInfo, error)         { return f.ports, nil }
func (f fakeProvider) Open(string, Config) (Port, error) { return nil, nil }

func TestEnumeratorListsPortsAsDevices(t *testing.T) {
	e := Enumerator{P: fakeProvider{ports: []PortInfo{
		{Name: "COM7", IsUSB: true, VID: "05C6", PID: "9008", SerialNumber: "abc", Product: "Qualcomm HS-USB QDLoader 9008"},
	}}}
	if e.Kind() != device.KindSerial {
		t.Fatalf("kind = %s", e.Kind())
	}
	devs, err := e.List(context.Background())
	if err != nil || len(devs) != 1 || devs[0].ID() != "COM7" {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	info, err := devs[0].Info(context.Background())
	if err != nil || info.Model != "Qualcomm HS-USB QDLoader 9008" || info.Extra["vid"] != "05C6" || info.Extra["pid"] != "9008" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}
