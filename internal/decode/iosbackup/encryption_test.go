package iosbackup_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
	"github.com/rbenzing/minutiae/internal/parse"
)

const xmlHead = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0">`

// Binary plists, produced by (and False for the second):
//
//	py -3 -c "import plistlib,sys;sys.stdout.write(plistlib.dumps({'IsEncrypted':True},fmt=plistlib.FMT_BINARY).hex())"
const (
	binTrue  = "62706c6973743030d101025b4973456e6372797074656409080b170000000000000101000000000000000300000000000000000000000000000018"
	binFalse = "62706c6973743030d101025b4973456e6372797074656408080b170000000000000101000000000000000300000000000000000000000000000018"
)

func xmlDict(body string) []byte { return []byte(xmlHead + "<dict>" + body + "</dict></plist>") }

func plistSeeds() [][]byte {
	b1, _ := hex.DecodeString(binTrue)
	b2, _ := hex.DecodeString(binFalse)
	return [][]byte{
		b1, b2,
		xmlDict(`<key>IsEncrypted</key><true/>`),
		xmlDict(`<key>IsEncrypted</key><false/>`),
		xmlDict(`<key>Other</key><true/>`),
		xmlDict(`<key>IsEncrypted</key><string>true</string>`),
		xmlDict(`<key>IsEncrypted</key><true/><key>IsEncrypted</key><false/>`),
		[]byte("garbage"),
	}
}

func TestReadEncryption(t *testing.T) {
	bin := func(h string) []byte { b, _ := hex.DecodeString(h); return b }
	cases := []struct {
		name   string
		in     []byte
		want   iosbackup.Encryption
		reason string
	}{
		{"xml true", xmlDict(`<key>IsEncrypted</key><true/>`), iosbackup.EncYes, ""},
		{"xml false", xmlDict(`<key>IsEncrypted</key><false/>`), iosbackup.EncNo, ""},
		{"binary true", bin(binTrue), iosbackup.EncYes, ""},
		{"binary false", bin(binFalse), iosbackup.EncNo, ""},
		{"key absent", xmlDict(`<key>Other</key><true/>`), iosbackup.EncUnknown, "no IsEncrypted key"},
		{"string true", xmlDict(`<key>IsEncrypted</key><string>true</string>`), iosbackup.EncUnknown, "not a boolean"},
		{"integer one", xmlDict(`<key>IsEncrypted</key><integer>1</integer>`), iosbackup.EncUnknown, "not a boolean"},
		{"key twice", xmlDict(`<key>IsEncrypted</key><true/><key>IsEncrypted</key><false/>`), iosbackup.EncUnknown, "Manifest.plist unreadable"},
		{"garbage", []byte("garbage"), iosbackup.EncUnknown, "Manifest.plist unreadable"},
		{"empty", nil, iosbackup.EncUnknown, "Manifest.plist unreadable"},
		{"array root", []byte(xmlHead + `<array><true/></array></plist>`), iosbackup.EncUnknown, "not a dictionary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := iosbackup.ReadEncryption(c.in, parse.NewBudget(1<<30).View())
			if got != c.want || (c.reason == "" && why != "") || !strings.Contains(why, c.reason) {
				t.Errorf("= %v %q, want %v containing %q", got, why, c.want, c.reason)
			}
		})
	}
}

func TestReadEncryptionNeverDefaultsToNotEncrypted(t *testing.T) {
	var zero iosbackup.Encryption
	if zero != iosbackup.EncUnknown || zero.String() != "unknown" {
		t.Errorf("the zero Encryption is %v %q, want unknown", zero, zero.String())
	}
	for e, s := range map[iosbackup.Encryption]string{iosbackup.EncNo: "not-encrypted", iosbackup.EncYes: "encrypted", iosbackup.EncUnknown: "unknown"} {
		if e.String() != s {
			t.Errorf("%d.String() = %q, want %q", e, e.String(), s)
		}
	}
	// A refused budget is undecidable, not "not encrypted".
	if got, why := iosbackup.ReadEncryption(plistSeeds()[3], parse.NewBudget(0).View()); got != iosbackup.EncUnknown || why == "" {
		t.Errorf("budget 0: %v %q, want unknown with a reason", got, why)
	}
}

func TestReadEncryptionRefusesAnOversizePlist(t *testing.T) {
	valid := xmlDict(`<key>IsEncrypted</key><false/>`)
	pad := func(n int) []byte { return append(bytes.Clone(valid), bytes.Repeat([]byte(" "), n-len(valid))...) }
	// A budget of 0 would fail any decode: the decision is made before one.
	got, why := iosbackup.ReadEncryption(pad(iosbackup.MaxManifestPlistBytes+1), parse.NewBudget(0).View())
	if got != iosbackup.EncUnknown || !strings.Contains(why, "too large") || !strings.Contains(why, "16777217 bytes") || !strings.Contains(why, "limit 16777216") {
		t.Errorf("= %v %q, want unknown, too large (16777217 bytes, limit 16777216)", got, why)
	}
	// Exactly at the limit it is decoded and decided.
	if got, why := iosbackup.ReadEncryption(pad(iosbackup.MaxManifestPlistBytes), parse.NewBudget(1<<30).View()); got != iosbackup.EncNo {
		t.Errorf("at the limit = %v %q, want not-encrypted", got, why)
	}
}
