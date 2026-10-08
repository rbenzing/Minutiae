package mb2

import (
	"errors"
	"strings"
	"testing"
)

// FB-2: every error UnmarshalPlist returns carries the "mb2: " prefix, also one the decoding
// library raises after the validator or pre-scan passed the document.
func TestUnmarshalPlistErrorsKeepThePrefix(t *testing.T) {
	var v any
	// binary: the set tag passes the structural check and the library refuses it
	if err := UnmarshalPlist(bplist([]byte{0xC0}), &v); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
		t.Fatalf("set tag: %v", err)
	}
	// XML: well formed for the pre-scan, refused by the library (a bad date)
	doc := `<plist version="1.0"><dict><key>a</key><date>not a date</date></dict></plist>`
	if err := UnmarshalPlist([]byte(doc), &v); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
		t.Fatalf("bad date: %v", err)
	}
	// the cause stays reachable
	orig := unmarshal
	t.Cleanup(func() { unmarshal = orig })
	cause := errors.New("decoder says no")
	unmarshal = func([]byte, any) (int, error) { return 0, cause }
	err := UnmarshalPlist([]byte("<plist/>"), &v)
	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "mb2: ") {
		t.Fatalf("decoder error: %v", err)
	}
	// a recovered decoder panic uses the same prefix
	unmarshal = func([]byte, any) (int, error) { panic("boom") }
	err = UnmarshalPlist([]byte("<plist/>"), &v)
	if err == nil || !strings.HasPrefix(err.Error(), "mb2: ") || strings.Contains(err.Error(), "mobilebackup2") {
		t.Fatalf("panic: %v", err)
	}
	// OpenStep data that looks like a tag is refused with the prefix too
	unmarshal = orig
	if err := UnmarshalPlist([]byte("<deadbeef>"), &v); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
		t.Fatalf("<deadbeef>: %v", err)
	}
}
