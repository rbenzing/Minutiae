//go:build windows

package examine

import "testing"

// The space check uses the bytes available to the caller (a quota applies), never the volume total or
// the total free bytes.
func TestCallerAvailableIsTheQuotaAwareNumber(t *testing.T) {
	for _, tc := range []struct{ free, total, totalFree uint64 }{
		{10, 100, 50},
		{0, 100, 50},
		{50, 100, 50},
		{7, 7, 90},
	} {
		if got := callerAvailable(tc.free, tc.total, tc.totalFree); got != tc.free {
			t.Errorf("callerAvailable(%d, %d, %d) = %d, want the caller-available %d", tc.free, tc.total, tc.totalFree, got, tc.free)
		}
	}
}

func TestDiskFreeNeverExceedsTotalFree(t *testing.T) {
	dir := t.TempDir()
	free, total, totalFree, err := diskFreeEx(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := diskFree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(got) > totalFree || uint64(got) > total {
		t.Errorf("diskFree = %d exceeds total free %d (volume %d)", got, totalFree, total)
	}
	_ = free
}
