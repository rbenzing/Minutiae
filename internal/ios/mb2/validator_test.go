package mb2

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/plist"
)

// The numbers of the validator this package used before it delegated to decode/plist, written
// as literals so a change of mb2Limits cannot go unnoticed.
func TestMb2ValidatorLimitsAreUnchanged(t *testing.T) {
	want := plist.Limits{MaxNodes: 1048576, MaxDepth: 64, MaxPayload: 67108864}
	if got := mb2Limits(); got != want {
		t.Fatalf("mb2Limits() = %+v, want %+v", got, want)
	}
}

func TestUnmarshalPlistRefusesHostileXML(t *testing.T) {
	deep := `<?xml version="1.0"?><plist version="1.0">` +
		strings.Repeat("<array>", 65) + strings.Repeat("</array>", 65) + `</plist>`
	entity := `<?xml version="1.0"?><!DOCTYPE plist [<!ENTITY a "aaaa">]><plist version="1.0"><dict><key>k</key><string>&a;</string></dict></plist>`
	for name, in := range map[string]string{"depth-65": deep, "entity-doctype": entity} {
		t.Run(name, func(t *testing.T) {
			var v any
			err := UnmarshalPlist([]byte(in), &v)
			if err == nil {
				t.Fatalf("accepted: %+v", v)
			}
			if !strings.HasPrefix(err.Error(), "mb2: ") {
				t.Fatalf("error lacks the mb2 prefix: %v", err)
			}
		})
	}
	ok := `<?xml version="1.0"?><plist version="1.0"><dict><key>k</key><string>v</string></dict></plist>`
	var m map[string]string
	if err := UnmarshalPlist([]byte(ok), &m); err != nil || m["k"] != "v" {
		t.Fatalf("legit xml: %v %v", m, err)
	}
}

func TestRecvStillRejectsXMLFrame(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?><plist version="1.0"><array><string>x</string></array></plist>`)
	buf := bytes.NewBuffer(binary.BigEndian.AppendUint32(nil, uint32(len(xml))))
	buf.Write(xml)
	if msg, err := NewCodec(buf).Recv(); err == nil {
		t.Fatalf("accepted an XML frame: %v", msg)
	}
}
