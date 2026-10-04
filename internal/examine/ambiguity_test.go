package examine_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// An HFS+ volume that also carries a FAT boot sector in its boot blocks (a stale
// one from an earlier format) opens as HFS+, and the partition says that the FAT
// driver matched too, the way the fall-through note is surfaced.
func TestInfoReportsAmbiguousSignatures(t *testing.T) {
	fatImg := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("fat")}})
	hfs := hfsplustest.Build(hfsplustest.Options{Label: "AMBIG"}, []hfsplustest.File{{Path: "/hello.txt", Data: []byte("hello")}})
	copy(hfs, fatImg[:512]) // the FAT16 boot sector in the HFS+ boot blocks

	s, _ := imageSession(t, hfs)
	p := onlyPartition(t, s)
	if p.FSType != "hfsplus" || p.FSInfo == nil {
		t.Fatalf("partition = %+v, want an hfsplus filesystem", p)
	}
	if !strings.Contains(p.Error, "also matched by: fat (ambiguous signatures)") {
		t.Errorf("Error = %q, want the ambiguity note naming fat", p.Error)
	}
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Lookup("/hello.txt"); err != nil {
		t.Errorf("Lookup = %v", err)
	}
}

// A clean image carries no ambiguity note, and a valid HFS+ header planted in a
// FAT volume falls through to FAT with the failure note and no ambiguity note
// (the failed driver is not an "also matched" one).
func TestInfoHasNoAmbiguityNoteWhenClean(t *testing.T) {
	hfs := hfsplustest.Build(hfsplustest.Options{Label: "CLEAN"}, []hfsplustest.File{{Path: "/hello.txt", Data: []byte("hello")}})
	s, _ := imageSession(t, hfs)
	if p := onlyPartition(t, s); p.FSType != "hfsplus" || p.Error != "" {
		t.Errorf("clean partition = %+v", p)
	}

	planted := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("fat")}})
	copy(planted[1024:], hfs[1024:1024+512])
	s, _ = imageSession(t, planted)
	p := onlyPartition(t, s)
	if p.FSType != "fat16" || p.FSInfo == nil {
		t.Fatalf("partition = %+v, want fat16", p)
	}
	if !strings.Contains(p.Error, "driver hfsplus matched but failed to open") || !strings.Contains(p.Error, "opened as fat") || strings.Contains(p.Error, "also matched") {
		t.Errorf("Error = %q, want the hfsplus failure, \"opened as fat\" and no ambiguity note", p.Error)
	}
}
