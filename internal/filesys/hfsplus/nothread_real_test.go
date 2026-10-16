package hfsplus_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// threadPattern is the start of a file-thread record in a catalog leaf: key length 6,
// the file's id as the key's parent, an empty name, then record type 4 (kHFSPlusFileThreadRecord).
func threadPattern(cnid uint32) []byte {
	p := []byte{0, 6, 0, 0, 0, 0, 0, 0, 0, 4}
	binary.BigEndian.PutUint32(p[2:], cnid)
	return p
}

// The builder is not the oracle: the kernel-written populated volume has ONE thread record
// damaged in place, and the file's bytes must still equal the independent oracle's SHA-256.
func TestOpenNoThreadFileOnRealImage(t *testing.T) {
	img, _ := loadFixture(t, "hfsplus-populated")
	ex := loadOracle(t, "hfsplus-populated")
	var target *fxFile
	for i := range ex.Files {
		o := &ex.Files[i]
		if o.Type == "file" && !o.HardLink && !o.Private && o.Size > 0 && o.Size <= 1<<20 && o.SHA256 != "" {
			if bytes.Count(img, threadPattern(o.CNID)) == 1 {
				target = o
				break
			}
		}
	}
	if target == nil {
		t.Fatal("no plain file of the populated oracle has exactly one thread record in the image")
	}

	check := func(f opener, label string) {
		t.Helper()
		fl, err := f.Open(filesys.Entry{ID: "cnid:" + itoa64(int64(target.CNID))})
		if err != nil {
			t.Fatalf("%s: Open %s: %v", label, target.Path, err)
		}
		b := make([]byte, fl.Size())
		if n, _ := fl.ReadAt(b, 0); n != len(b) {
			t.Fatalf("%s: short read", label)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != target.SHA256 || int64(len(b)) != target.Size {
			t.Errorf("%s: %s sha256 %x (%d bytes), oracle %s (%d bytes)", label, target.Path, sum, len(b), target.SHA256, target.Size)
		}
	}
	// Control: the intact image opens the file through its thread.
	check(open(t, img), "intact")

	// Damage: record type 0x00FF is unknown to every reader and makes the thread UNREADABLE (never
	// "missing": the record is there). The open skips its own thread position with a warning and
	// the catalog scan proves the id unique. Nothing else is changed.
	dmg := bytes.Clone(img)
	at := bytes.Index(dmg, threadPattern(target.CNID))
	dmg[at+9] = 0xFF
	f := open(t, dmg)
	e, err := f.Lookup(target.Path)
	if err != nil {
		t.Fatalf("Lookup(%s) on the damaged image: %v", target.Path, err)
	}
	if v, _ := attr(e, "thread"); v != "unreadable" {
		t.Fatalf("%s: attrs %+v, want thread=unreadable", target.Path, e.Attrs)
	}
	check(f, "no thread")
	if !hasWarning(f.Info(), "unknown type") {
		t.Errorf("the skipped record must be warned about: %q", f.Info().Warnings)
	}

	// An id the damaged catalog does not hold at all stays ErrNotFound (never a default).
	if _, err := f.Open(filesys.Entry{ID: "cnid:4294967000"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an unused id: %v, want ErrNotFound", err)
	}
}

type opener interface {
	Open(filesys.Entry) (filesys.File, error)
}
