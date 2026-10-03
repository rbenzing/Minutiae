package mb2

import (
	"bytes"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestUnmarshalPlist(t *testing.T) {
	bin, err := plist.Marshal(map[string]any{"SnapshotState": "finished"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	xml := []byte("\xef\xbb\xbf\n" + `<?xml version="1.0"?><plist version="1.0"><dict><key>SnapshotState</key><string>finished</string></dict></plist>`)
	for _, tc := range []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"binary", bin, true},
		{"xml", xml, true},
		{"bplist01-prefix-is-validated", append([]byte("bplist01"), bin[8:]...), false},
		{"openstep-text", []byte(`{ SnapshotState = finished; }`), false},
		{"oversized", append(xml, bytes.Repeat([]byte(" "), MaxPlistFile)...), false},
		{"empty", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st struct {
				SnapshotState string `plist:"SnapshotState"`
			}
			err := UnmarshalPlist(tc.in, &st)
			if tc.ok && (err != nil || st.SnapshotState != "finished") {
				t.Fatalf("state=%q err=%v", st.SnapshotState, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("accepted: %+v", st)
			}
		})
	}
}

func TestUnmarshalPlistConvertsDecoderPanicToError(t *testing.T) {
	orig := unmarshal
	t.Cleanup(func() { unmarshal = orig })
	unmarshal = func([]byte, any) (int, error) { panic("boom") }
	var v any
	if err := UnmarshalPlist([]byte("<plist/>"), &v); err == nil || !strings.Contains(err.Error(), "malformed plist") {
		t.Fatalf("err = %v", err)
	}
}
