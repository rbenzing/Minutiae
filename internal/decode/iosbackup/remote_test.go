package iosbackup_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
)

func TestSplitRemote(t *testing.T) {
	const id = "3d0d7e5fb2ce288813306e4d4636395e047a3d28"
	cases := []struct {
		in       string
		dir, fid string
		ok       bool
	}{
		{"UDID1/3d/" + id, "UDID1", id, true},
		{"00008030-001C/3d/" + id, "00008030-001C", id, true},
		{"UDID1/3D/" + strings.ToUpper(id), "", "", false},
		{"UDID1/3d/" + strings.ToUpper(id), "", "", false},
		{"UDID1/aa/" + id, "", "", false},
		{"UDID1/Snapshot/3d/" + id, "", "", false},
		{"Manifest.db", "", "", false},
		{"/3d/" + id, "", "", false},
		{"UDID1/3d/" + id + "/x", "", "", false},
		{"UDID1//" + id, "", "", false},
		{"UDID1/3d/" + id[:39], "", "", false},
		{"UDID1/3d/" + id + "0", "", "", false},
		{"UDID1/3d/g" + id[1:], "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		dir, f, ok := iosbackup.SplitRemote(c.in)
		if dir != c.dir || f != c.fid || ok != c.ok {
			t.Errorf("SplitRemote(%q) = %q %q %v, want %q %q %v", c.in, dir, f, ok, c.dir, c.fid, c.ok)
		}
	}
}

func TestValidFileIDAndDomain(t *testing.T) {
	good := "3d0d7e5fb2ce288813306e4d4636395e047a3d28"
	for id, want := range map[string]bool{good: true, strings.ToUpper(good): false, good[1:]: false, "": false, good[:39] + "z": false} {
		if got := iosbackup.ValidFileID(id); got != want {
			t.Errorf("ValidFileID(%q) = %v, want %v", id, got, want)
		}
	}
	for d, want := range map[string]bool{
		"HomeDomain": true, "AppDomain-com.example": true, "..a": true,
		"": false, ".": false, "..": false, "a/b": false, `a\b`: false, "a\x00": false,
		strings.Repeat("d", 255): true, strings.Repeat("d", 256): false, "\xff": false,
	} {
		if got := iosbackup.ValidDomain(d); got != want {
			t.Errorf("ValidDomain(%q) = %v, want %v", d, got, want)
		}
	}
	for p, want := range map[string]bool{
		"a": true, "a/b/c.db": true, ".hidden": true, "a/.b": true, "a..b": true,
		"": false, ".": false, "..": false, "a/..": false, "../a": false, "a/./b": false, "a//b": false,
		"/a": false, "a/": false, "a\x00b": false, `a\b`: false, "a\xffb": false,
	} {
		if got := iosbackup.ValidRelativePath(p); got != want {
			t.Errorf("ValidRelativePath(%q) = %v, want %v", p, got, want)
		}
	}
}
