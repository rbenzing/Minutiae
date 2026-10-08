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

// bplist builds a one-object binary plist (offset and reference size 1) around obj.
func bplist(obj []byte) []byte {
	b := append([]byte("bplist00"), obj...)
	tableOff := len(b)
	b = append(b, 8) // offset of object 0
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], 1)
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

// The binary checks decode/plist applies are stricter than the validator mb2 used to have;
// each is a refusal that keeps the "mb2: " prefix of every error this package returns.
func TestMb2StricterBinaryChecksAreRefusals(t *testing.T) {
	int128 := append([]byte{0x14}, bytes.Repeat([]byte{0x01}, 16)...) // hi is not 0
	cases := map[string][]byte{
		"128-bit integer":  bplist(int128),
		"scalar past area": bplist([]byte{0x13, 0x00}), // 8-byte integer, 1 byte present
		"string past area": bplist([]byte{0x5f, 0x10, 0x40, 'a'}),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkBinary(in); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
				t.Fatalf("checkBinary: %v, want an mb2: refusal", err)
			}
			var v any
			if err := UnmarshalPlist(in, &v); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
				t.Fatalf("UnmarshalPlist: %v (%v), want an mb2: refusal", err, v)
			}
			buf := bytes.NewBuffer(binary.BigEndian.AppendUint32(nil, uint32(len(in))))
			buf.Write(in)
			if msg, err := NewCodec(buf).Recv(); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
				t.Fatalf("Recv: %v (%v), want an mb2: refusal", err, msg)
			}
		})
	}
}

// trimXMLLead removes exactly one BOM plus ASCII space, tab, CR and LF; any other lead is
// not an XML plist at all and is refused (P43).
func TestUnmarshalPlistRefusesOddLeads(t *testing.T) {
	doc := `<?xml version="1.0"?><plist version="1.0"><dict><key>k</key><string>v</string></dict></plist>`
	const bom = "\xef\xbb\xbf"
	for name, lead := range map[string]string{
		"second BOM":      bom + bom,
		"NUL":             "\x00",
		"NBSP":            " ",
		"EM SPACE":        " ",
		"form feed":       "\f",
		"vtab":            "\v",
		"BOM and NUL":     bom + "\x00",
		"BOM, space, BOM": bom + " " + bom,
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]string
			err := UnmarshalPlist([]byte(lead+doc), &m)
			if err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
				t.Fatalf("accepted or lacks the prefix: %v %v", m, err)
			}
		})
	}
	for name, lead := range map[string]string{"none": "", "BOM": bom, "ws": " \t\r\n", "BOM ws": bom + " \t\r\n"} {
		var m map[string]string
		if err := UnmarshalPlist([]byte(lead+doc), &m); err != nil || m["k"] != "v" {
			t.Fatalf("%s: %v %v", name, m, err)
		}
	}
}

// The set tag 0xC passes the structural check (the walker bounds it like an array) and is
// refused by the decoder, which has no set type. Over DeviceLink the refusal keeps the "mb2: "
// prefix; UnmarshalPlist returns the decoder's own error, so only the refusal is pinned there.
func TestMb2RefusesSetTag(t *testing.T) {
	in := bplist([]byte{0xC0})
	buf := bytes.NewBuffer(binary.BigEndian.AppendUint32(nil, uint32(len(in))))
	buf.Write(in)
	if msg, err := NewCodec(buf).Recv(); err == nil || !strings.HasPrefix(err.Error(), "mb2: ") {
		t.Fatalf("Recv: %v (%v), want an mb2: refusal", err, msg)
	}
	var v any
	if err := UnmarshalPlist(in, &v); err == nil {
		t.Fatalf("UnmarshalPlist accepted a set: %#v", v)
	}
}
