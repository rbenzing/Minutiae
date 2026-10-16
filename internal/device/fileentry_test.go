package device_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
)

func TestFileEntryUnknownTimeIsNull(t *testing.T) {
	b, err := json.Marshal(device.FileEntry{Name: "a", Path: "/a", Size: 1})
	if err != nil || !strings.Contains(string(b), `"mod_time":null`) || strings.Contains(string(b), "0001-01-01") {
		t.Fatalf("unknown time: %s, %v", b, err)
	}
	b, err = json.Marshal(device.FileEntry{Name: "a", ModTime: time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)})
	if err != nil || !strings.Contains(string(b), `"mod_time":"2023-11-14T22:13:20Z"`) {
		t.Fatalf("known time: %s, %v", b, err)
	}
}
