package sqlitefile_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestSniffKnownContainersAreNotHintedAsEncrypted: high-entropy content that
// starts with the magic of a common compressed, archive or media format is not
// offered as "looks encrypted" (the hint is never an assertion, and an examiner
// may repeat it). A one-byte prefix is not a magic: random content that merely
// starts with a brace keeps the hint.
func TestSniffKnownContainersAreNotHintedAsEncrypted(t *testing.T) {
	for name, magic := range map[string]string{
		"gzip": "\x1f\x8b", "zip": "PK\x03\x04", "zip empty": "PK\x05\x06", "png": "\x89PNG", "jpeg": "\xff\xd8\xff",
		"bplist": "bplist00", "xml": "<?xml", "zstd": "\x28\xb5\x2f\xfd", "xz": "\xfd7zXZ\x00", "bzip2": "BZh9",
		"7z": "7z\xbc\xaf\x27\x1c", "rar": "Rar!\x1a\x07", "pdf": "%PDF-1.7", "ogg": "OggS", "matroska": "\x1a\x45\xdf\xa3",
		"gif": "GIF89a", "riff": "RIFF", "mp3 tag": "ID3",
		"mp4": "\x00\x00\x00\x18ftypmp42",
	} {
		data := append([]byte(magic), pseudoRandom(4096, 7)...)
		if s, err := sqlitefile.Sniff(bytes.NewReader(data), int64(len(data))); err != nil || s.Kind == sqlitefile.SniffLooksEncrypted {
			t.Errorf("%s: kind %v, err %v: a known container must not carry the encryption hint", name, s.Kind, err)
		}
	}
	for _, first := range []string{"{", "["} {
		data := append([]byte(first), pseudoRandom(4096, 9)...)
		if s, err := sqlitefile.Sniff(bytes.NewReader(data), int64(len(data))); err != nil || s.Kind != sqlitefile.SniffLooksEncrypted {
			t.Errorf("random content starting with %q: kind %v, err %v, want the hint (one byte is not a magic)", first, s.Kind, err)
		}
	}
}
