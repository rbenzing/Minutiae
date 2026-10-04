package apfs_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/apfs"
)

func TestParseEntryID(t *testing.T) {
	for _, tc := range []struct {
		id        string
		kind      string
		vol       int
		view, ino uint64
	}{
		{"apfs:root", "root", 0, 0, 0},
		{"n:0:0:2", "node", 0, 0, 2},
		{"n:99:0:1", "node", 99, 0, 1},
		{"n:7:12:16", "node", 7, 12, 16},
		{"n:0:0:1152921504606846975", "node", 0, 0, 1<<60 - 1},
		{"n:0:18446744073709551615:2", "node", 0, 1<<64 - 1, 2}, // a view is only checked against the disk
		{"snaps:0", "snaps", 0, 0, 0},
		{"snaps:99", "snaps", 99, 0, 0},
	} {
		kind, vol, view, ino, ok := apfs.ParseEntryID(tc.id)
		if !ok || kind != tc.kind || vol != tc.vol || view != tc.view || ino != tc.ino {
			t.Errorf("ParseEntryID(%q) = %q %d %d %d %v, want %q %d %d %d", tc.id, kind, vol, view, ino, ok, tc.kind, tc.vol, tc.view, tc.ino)
		}
		// Every ID the reader builds round-trips.
		if tc.kind == "node" {
			if got := apfs.NodeID(tc.vol, tc.view, tc.ino); got != tc.id {
				t.Errorf("NodeID = %q, want %q", got, tc.id)
			}
		}
	}

	for _, id := range []string{
		"", " ", "apfs:root ", " apfs:root", "APFS:ROOT", "apfs:Root", "apfs:", "apfs:roo",
		"n:", "n:0", "n:0:0", "n:0:0:", "n::0:2", "n:0::2", "n:0:0:2:", "n:0:0:2:0", "n:0:0:2 ", " n:0:0:2", "n:0:0:2\n",
		"n:00:0:2", "n:0:00:2", "n:0:0:02", "n:01:0:2", // leading zeros
		"n:+0:0:2", "n:0:+0:2", "n:0:0:+2", "n:-0:0:2", "n:0:-1:2", "n:0:0:-2", // signs
		"n: 0:0:2", "n:0: 0:2", "n:0:0: 2", "n:0:0:2 x", // spaces
		"n:0x0:0:2", "n:0:0x1:2", "n:0:0:0x2", "n:0:0:0b10", "n:0:0:1_0", // radix and separators
		"n:0:0:99999999999999999999", "n:0:18446744073709551616:2", "n:99999999999999999999:0:2", // overflow
		"N:0:0:2", "x:0:0:2", "node:0:0:2", "inode:2", "nid:2", "n-0-0-2", // prefix
		"n:100:0:2", "n:1000:0:2", // volume out of range
		"n:0:0:0", "n:0:0:1152921504606846976", "n:0:0:1152921504606846977", // ino 0 or beyond 2^60-1
		"n:0:0:٢", "n:0:0:2.0", "n:0:0:2e0", // non-ASCII digits and floats
		"snaps:", "snaps:100", "snaps:00", "snaps:01", "snaps:+1", "snaps:-1", "snaps: 1", "snaps:1 ", "snaps:1:0", "Snaps:1", "snaps:0x1",
	} {
		if kind, vol, view, ino, ok := apfs.ParseEntryID(id); ok {
			t.Errorf("ParseEntryID(%q) accepted: %q %d %d %d", id, kind, vol, view, ino)
		}
	}

	// Strictly: every canonical ID in range parses, nothing else does (sampled).
	for vol := 0; vol < 120; vol += 7 {
		for _, ino := range []uint64{0, 1, 2, 9, 10, 100, 1<<60 - 1, 1 << 60} {
			id := fmt.Sprintf("n:%d:0:%d", vol, ino)
			_, _, _, _, ok := apfs.ParseEntryID(id)
			if want := vol < 100 && ino >= 1 && ino <= 1<<60-1; ok != want {
				t.Errorf("ParseEntryID(%q) = %v, want %v", id, ok, want)
			}
		}
	}
}
