package mb2

import (
	"path/filepath"
	"testing"
)

func TestInsideOrSame(t *testing.T) {
	j := filepath.Join
	for _, tc := range []struct {
		src, dst string
		fold     bool
		want     bool
	}{
		{j("s", "U1", "dir"), j("s", "U1", "dir"), false, true},
		{j("s", "U1", "dir"), j("s", "U1", "dir", "sub"), false, true},
		{j("s", "U1", "dir"), j("s", "u1", "DIR", "sub"), true, true}, // case-insensitive FS: same tree
		{j("s", "U1", "dir"), j("s", "u1", "DIR"), true, true},
		{j("s", "U1", "dir"), j("s", "u1", "DIR", "sub"), false, false},
		{j("s", "U1", "dir"), j("s", "U1", "dirx"), true, false},
		{j("s", "U1", "dir"), j("s", "U1"), true, false},
	} {
		if got := insideOrSame(tc.src, tc.dst, tc.fold); got != tc.want {
			t.Errorf("insideOrSame(%q, %q, fold=%t) = %t", tc.src, tc.dst, tc.fold, got)
		}
	}
}
