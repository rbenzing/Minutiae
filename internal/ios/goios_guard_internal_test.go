package ios

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/afc"
)

// restoreSeams puts the go-ios seams back when the test ends.
func restoreSeams(t *testing.T) {
	t.Helper()
	l, v, c, a := goiosListDevices, goiosValues, goiosConnect, goiosAFC
	t.Cleanup(func() { goiosListDevices, goiosValues, goiosConnect, goiosAFC = l, v, c, a })
}

func wantRecoveredPanic(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("%s: err = %v, want the panic returned as an error", what, err)
	}
}

func TestGoIOSLockdownValuesPanicIsAnError(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	goiosValues = func(goios.DeviceEntry) (map[string]any, error) { panic("hostile lockdown plist") }
	_, err := (GoIOS{}).Values(context.Background(), "U1")
	wantRecoveredPanic(t, "Values", err)

	// Through the device layer: ios info returns an error and does not crash.
	devs, err := Enumerator{B: GoIOS{}}.List(context.Background())
	if err != nil || len(devs) == 0 {
		t.Fatalf("List = %v, %v", devs, err)
	}
	_, err = devs[0].(*Device).Info(context.Background())
	wantRecoveredPanic(t, "Info", err)
}

func TestGoIOSStartServicePanicIsAnError(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	goiosConnect = func(goios.DeviceEntry, string) (io.ReadWriteCloser, error) { panic("hostile StartService plist") }
	goiosAFC = func(goios.DeviceEntry) (*afc.Client, error) { panic("hostile StartService plist") }
	a, err := (GoIOS{}).OpenAFC(context.Background(), "U1")
	if a != nil {
		t.Fatalf("OpenAFC returned a client %v with a panic", a)
	}
	wantRecoveredPanic(t, "OpenAFC", err)
	rw, err := (GoIOS{}).OpenBackup(context.Background(), "U1")
	if rw != nil {
		t.Fatalf("OpenBackup returned a connection %v with a panic", rw)
	}
	wantRecoveredPanic(t, "OpenBackup", err)
}

func TestGoIOSUsbmuxdListPanicIsAnError(t *testing.T) {
	restoreSeams(t)
	goiosListDevices = func() ([]goios.DeviceEntry, error) { panic("hostile usbmuxd plist") }
	_, err := (GoIOS{}).List(context.Background())
	wantRecoveredPanic(t, "List", err)
	_, err = (GoIOS{}).Values(context.Background(), "U1")
	wantRecoveredPanic(t, "Values lookup", err)
	_, err = entry("U1")
	wantRecoveredPanic(t, "entry", err)
}

func TestGoIOSGuardKeepsOrdinaryErrors(t *testing.T) {
	dualHomedU1(t)
	restoreSeams(t)
	boom := errors.New("lockdown said no")
	goiosValues = func(goios.DeviceEntry) (map[string]any, error) { return nil, boom }
	if _, err := (GoIOS{}).Values(context.Background(), "U1"); !errors.Is(err, boom) || strings.Contains(err.Error(), "panic") {
		t.Fatalf("an ordinary error must pass through unchanged: %v", err)
	}
	goiosValues = func(goios.DeviceEntry) (map[string]any, error) {
		return map[string]any{"ProductType": "iPhone14,2"}, nil
	}
	vals, err := (GoIOS{}).Values(context.Background(), "U1")
	if err != nil || vals["ProductType"] != "iPhone14,2" {
		t.Fatalf("Values = %v, %v", vals, err)
	}
}

// W25: the panic value is device-influenced text; it is quoted and bounded.
func TestGoIOSRecoveredPanicValueIsQuotedAndBounded(t *testing.T) {
	restoreSeams(t)
	hostile := "line1\nline2\x1b[31m\x00" + strings.Repeat("A", 500)
	goiosValues = func(goios.DeviceEntry) (map[string]any, error) { panic(hostile) }
	dualHomedU1(t)
	_, err := (GoIOS{}).Values(context.Background(), "U1")
	wantRecoveredPanic(t, "Values", err)
	msg := err.Error()
	for _, r := range msg {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control byte %#x in %q", r, msg)
		}
	}
	if strings.Contains(msg, strings.Repeat("A", 300)) {
		t.Fatalf("panic value not truncated: %d bytes", len(msg))
	}
	if !strings.Contains(msg, `line1\nline2\x1b[31m\x00`) {
		t.Fatalf("panic value not %%q-quoted: %q", msg)
	}
}

// W27: the default goiosConnect returns a true nil interface on error, never a
// typed nil that would pass a `!= nil` check.
func TestGoIOSDefaultConnectErrorIsANilConnection(t *testing.T) {
	dualHomedU1(t) // the fake usbmuxd answers ListDevices only, so the connect fails
	rw, err := (GoIOS{}).OpenBackup(context.Background(), "U1")
	if err == nil {
		t.Fatal("OpenBackup against a usbmuxd that cannot connect must fail")
	}
	if rw != nil {
		t.Fatalf("OpenBackup returned a non-nil connection (%T) with an error", rw)
	}
}
