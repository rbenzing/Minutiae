package typedstream

import (
	"bytes"
	"errors"
	"testing"
)

// windowStream is the header, k filler bytes, then tail. The MaxScan bytes after the header
// are the scan window (offsets 0..MaxScan-1); a marker may start at any of them.
func windowStream(k int, tail string) []byte {
	b := header()
	b = append(b, make([]byte, k)...)
	return append(b, tail...)
}

const (
	fullString = "\x08NSString\x01+\x03abc"
	cutString  = "\x08NSStr" // the input ends inside the started marker
)

func TestScanWindowEdges(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string // "", "trunc", "limit"
	}{
		{"zeros MaxScan-1", windowStream(MaxScan-1, ""), "trunc"},
		{"zeros MaxScan", windowStream(MaxScan, ""), ""},
		{"zeros MaxScan+1", windowStream(MaxScan+1, ""), ""},
		{"full marker at MaxScan-1", windowStream(MaxScan-1, fullString), "limit"},
		{"full marker at MaxScan", windowStream(MaxScan, fullString), ""},
		{"full marker at MaxScan+1", windowStream(MaxScan+1, fullString), ""},
		{"cut marker at MaxScan-1", windowStream(MaxScan-1, cutString), "trunc"},
		{"length byte only at MaxScan-1", windowStream(MaxScan-1, "\x08"), "trunc"},
		{"cut marker at MaxScan", windowStream(MaxScan, cutString), ""},
		{"cut marker at MaxScan+1", windowStream(MaxScan+1, cutString), ""},
		{"cut mutable marker at MaxScan-1", windowStream(MaxScan-1, "\x0fNSMutableStr"), "trunc"},
		{"cut marker at MaxScan-2", windowStream(MaxScan-2, cutString), "trunc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, ok, err := ExtractText(c.b, view(1<<20))
			if text != "" || ok {
				t.Fatalf("got (%q, %v), want no text", text, ok)
			}
			switch c.want {
			case "":
				if err != nil {
					t.Fatalf("want (false, nil), got %v", err)
				}
			case "trunc":
				if !errors.Is(err, ErrTruncated) {
					t.Fatalf("want ErrTruncated, got %v", err)
				}
			case "limit":
				if !errors.Is(err, ErrLimit) {
					t.Fatalf("want ErrLimit, got %v", err)
				}
			}
			compareToOracle(t, c.name, c.b)
		})
	}
}

func TestScanWindowMarkerStartingLastOffsetIsFoundInFull(t *testing.T) {
	// the marker starts at the last window byte and its text encoding lies inside the input
	// but past the window: ErrLimit, proving the marker was found and read in full.
	b := windowStream(MaxScan-1, fullString)
	if !bytes.Contains(b, []byte("NSString")) {
		t.Fatal("setup")
	}
	if _, _, err := ExtractText(b, view(1<<20)); !errors.Is(err, ErrLimit) {
		t.Fatalf("got %v", err)
	}
}
