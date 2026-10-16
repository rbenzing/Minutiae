package cli

import (
	"testing"
	"time"
)

func TestFormatModTimeUnknown(t *testing.T) {
	if got := formatModTime(time.Time{}); got != "unknown" {
		t.Fatalf("zero time = %q", got)
	}
	if got := formatModTime(time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)); got != "2023-11-14 22:13:20" {
		t.Fatalf("time = %q", got)
	}
}
