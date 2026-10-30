package iosbackup

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/decode/plist"
)

// MaxManifestPlistBytes is the largest Manifest.plist ReadEncryption decodes (4E-R36). A longer
// input is EncUnknown, decided before any decoding.
const MaxManifestPlistBytes = 16 << 20

// Encryption is what Manifest.plist says about the backup.
type Encryption uint8

// The encryption states. EncUnknown is the zero value: nothing is "not encrypted" by default.
const (
	EncUnknown Encryption = iota
	EncNo
	EncYes
)

// String names the state: "unknown", "not-encrypted" or "encrypted".
func (e Encryption) String() string {
	switch e {
	case EncNo:
		return "not-encrypted"
	case EncYes:
		return "encrypted"
	}
	return "unknown"
}

// ReadEncryption reads the IsEncrypted key of a Manifest.plist with the strict plist decoder. Only
// one boolean decides; every other outcome is EncUnknown with a reason: "no IsEncrypted key",
// "IsEncrypted is not a boolean", "Manifest.plist is not a dictionary", "Manifest.plist
// unreadable: <sentinel text>" (a repeated key included: which value counts cannot be decided) and
// "Manifest.plist is too large (<n> bytes, limit 16777216)". The reason is empty when decided.
func ReadEncryption(plistBytes []byte, b plist.Budget) (Encryption, string) {
	if len(plistBytes) > MaxManifestPlistBytes {
		return EncUnknown, fmt.Sprintf("Manifest.plist is too large (%d bytes, limit %d)", len(plistBytes), MaxManifestPlistBytes)
	}
	v, err := plist.Decode(plistBytes, b)
	if err != nil {
		return EncUnknown, "Manifest.plist unreadable: " + sentinelText(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return EncUnknown, "Manifest.plist is not a dictionary"
	}
	raw, ok := m["IsEncrypted"]
	if !ok {
		return EncUnknown, "no IsEncrypted key"
	}
	flag, ok := raw.(bool)
	if !ok {
		return EncUnknown, "IsEncrypted is not a boolean"
	}
	if flag {
		return EncYes, ""
	}
	return EncNo, ""
}

// sentinelText names the plist sentinel an error wraps, never the document's own text.
func sentinelText(err error) string {
	for _, s := range []error{plist.ErrMalformed, plist.ErrLimit, plist.ErrUnsupported, plist.ErrInternal, plist.ErrNoBudget} {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	return "unknown error"
}
