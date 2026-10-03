//go:build hardware

package ios_test

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/ios"
)

// Run manually with a trusted iPhone attached: go test -tags hardware ./internal/ios/ -v
func TestHardwareInfo(t *testing.T) {
	devs, err := ios.Enumerator{B: ios.GoIOS{}}.List(context.Background())
	if err != nil {
		t.Fatalf("usbmuxd: %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no iOS device attached")
	}
	info, err := devs[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", info)
}
