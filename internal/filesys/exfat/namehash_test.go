package exfat_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

func attrOf(e filesys.Entry, key string) string {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

func hasNameHashWarning(f *exfat.FS) bool {
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, "NameHash") {
			return true
		}
	}
	return false
}

func TestExfatNameHashSpecVector(t *testing.T) {
	ident := func(u uint16) uint16 { return u }
	upper := func(u uint16) uint16 {
		if u >= 'a' && u <= 'z' {
			return u - 32
		}
		return u
	}
	if got := exfat.NameHash(ident, []uint16{'A'}); got != 0x8020 {
		t.Errorf("nameHash(identity, A) = %#04x, want 0x8020", got)
	}
	if got := exfat.NameHash(upper, []uint16{'a'}); got != 0x8020 {
		t.Errorf("nameHash(upper, a) = %#04x, want 0x8020", got)
	}
	if got := exfat.NameHash(ident, nil); got != 0 {
		t.Errorf("nameHash of no units = %#04x, want 0", got)
	}
}

func TestExfatNameHashMismatchFlagsTheEntry(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/Hello.txt", Data: []byte("hello")}, {Path: "/other.txt", Data: []byte("x")},
	})
	loc := lay.Find("/Hello.txt", false)
	img[loc.Offset+32+4] ^= 0x01
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	e := byName(t, es, "Hello.txt")
	if got := attrOf(e, "name_hash"); got != "bad" {
		t.Errorf("name_hash = %q, want bad", got)
	}
	if got := attrOf(e, "checksum"); got != "" {
		t.Errorf("checksum = %q, want none", got)
	}
	if got := attrOf(byName(t, es, "other.txt"), "name_hash"); got != "" {
		t.Errorf("untouched entry name_hash = %q, want none", got)
	}
	var warn string
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, "NameHash") {
			warn = w
		}
	}
	if !strings.Contains(warn, "directory (first cluster") || !strings.Contains(warn, "entry ") || !strings.Contains(warn, "does not match the name") {
		t.Errorf("warning %q does not name directory, entry and mismatch", warn)
	}
	g, err := f.Lookup("/Hello.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	file, err := f.Open(g)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b := readFile(t, file); string(b) != "hello" {
		t.Errorf("content %q", b)
	}
}

func TestExfatNameHashDeletedSetChecked(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/gone.txt", Data: []byte("gone"), Deleted: true}, {Path: "/kept.txt", Data: []byte("x")},
	})
	loc := lay.Find("/gone.txt", true)
	img[loc.Offset+32+5] ^= 0x80
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	e := byName(t, es, "gone.txt")
	if !e.Deleted || attrOf(e, "name_hash") != "bad" {
		t.Errorf("deleted entry = %+v, want deleted with name_hash=bad", e)
	}
	if !hasNameHashWarning(f) {
		t.Error("no NameHash warning for the deleted set")
	}
}

func TestExfatNameHashUnverifiedWithoutUpcaseTable(t *testing.T) {
	img := exfattest.Build(exfattest.Options{NoUpcase: true}, []exfattest.File{
		{Path: "/zebra", Data: []byte("x")}, {Path: "/gone", Data: []byte("y"), Deleted: true},
	})
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range es {
		if e.Name != "zebra" && e.Name != "gone" {
			continue
		}
		n++
		if got := attrOf(e, "name_hash"); got != "unverified" {
			t.Errorf("%s: name_hash = %q, want unverified", e.Name, got)
		}
	}
	if n != 2 {
		t.Fatalf("saw %d of the 2 entries in %v", n, names(es))
	}
	if hasNameHashWarning(f) {
		t.Error("a mismatch warning without a table")
	}
}

func TestExfatNameHashTruncatedNameUnverified(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/aaaaaaaaaaaaaaa", Data: []byte("x")}})
	loc := lay.Find("/aaaaaaaaaaaaaaa", false)
	img[loc.Offset+32+3] = 20 // NameLength 20, one Name entry present (15 units)
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	e := byName(t, es, "aaaaaaaaaaaaaaa")
	if got := attrOf(e, "name_hash"); got != "unverified" {
		t.Errorf("name_hash = %q, want unverified", got)
	}
	if hasNameHashWarning(f) {
		t.Error("a mismatch warning for a name not read in full")
	}
}

// Pins existing behaviour: both committed real images verify in full.
// Mutation: hash with the identity table instead of the volume's.
func TestExfatNameHashVerifiesOnRealFixtures(t *testing.T) {
	totalNonASCII := 0
	defer func() {
		if totalNonASCII == 0 {
			t.Error("no walk saw a non-ASCII name")
		}
	}()
	for _, path := range []string{"testdata/exfat.img.gz", "testdata/recover/exfat-recover.img.gz"} {
		t.Run(path, func(t *testing.T) {
			img := gunzipFixture(t, path)
			raw, err := os.ReadFile(strings.TrimSuffix(path, ".img.gz") + ".expect.json")
			if err != nil {
				t.Fatal(err)
			}
			var o struct {
				Generator struct {
					ImageSHA256 string `json:"image_sha256"`
				} `json:"generator"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(img)
			if hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
				t.Fatal("image sha256 differs from the oracle: regenerate the fixtures")
			}
			fsys, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			seen, nonASCII, deleted := 0, 0, 0
			err = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("walk %s: %v", p, err)
					return nil
				}
				seen++
				if e.Deleted {
					deleted++
				}
				if strings.ContainsFunc(e.Name, func(r rune) bool { return r > 0x7F }) {
					nonASCII++
				}
				if v := attrOf(e, "name_hash"); v != "" {
					t.Errorf("%s: name_hash=%s on a real image (STOP: report, never adjust)", p, v)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			totalNonASCII += nonASCII
			if seen == 0 {
				t.Error("the walk saw no entry")
			}
			t.Logf("%d entries, %d deleted, %d non-ASCII", seen, deleted, nonASCII)
			if hasNameHashWarning(fsys) {
				t.Errorf("NameHash warnings: %v", fsys.Info().Warnings)
			}
		})
	}
}
