package detect_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// A valid HFS+ volume header planted where HFS+ keeps it (byte 1024 of the
// volume) inside a FAT or exFAT volume is handled by the fall-through, not by a
// special case in the HFS+ probe: the hfsplus driver matches (the header is a
// valid one) but its Open finds that the catalog the header points at is not a
// B-tree (the extents lie in FAT data), fails with a corrupt-structure error,
// and detection goes on to the FAT driver with the usual note. An exFAT volume
// is claimed by exfat first (it precedes hfsplus), which then lists hfsplus as
// an ambiguous claimant. A FAT32 volume's reserved sector 2 and an exFAT
// volume's extended boot sector 1 sit at byte 1024; on FAT12/16 it is the
// first FAT.
func TestPlantedHFSPlusHeaderFallsThroughToFAT(t *testing.T) {
	hfs := hfsplustest.Build(hfsplustest.Options{Label: "STALE"}, nil)
	header := hfs[1024 : 1024+512]
	for name, tc := range map[string]struct {
		path, typ   string
		fellThrough bool
	}{
		"fat12": {"../fat/testdata/fat12.img.gz", "fat12", true},
		"fat16": {"../fat/testdata/fat16.img.gz", "fat16", true},
		"fat32": {"../fat/testdata/fat32.img.gz", "fat32", true},
		"exfat": {"../exfat/testdata/exfat.img.gz", "exfat", false},
	} {
		t.Run(name, func(t *testing.T) {
			img := gunzipFixture(t, tc.path)
			copy(img[1024:], header)
			size := int64(len(img))
			r := bytes.NewReader(img)
			if !hfsplusProbe(r, size) {
				t.Fatal("the planted header is not seen by the hfsplus driver: the test plants nothing")
			}
			fsys, err := detect.Open(r, size)
			if err != nil {
				t.Fatal(err)
			}
			info := fsys.Info()
			if info.Type != tc.typ {
				t.Fatalf("Info().Type = %q, want %q", info.Type, tc.typ)
			}
			joined := strings.Join(info.Warnings, "\n")
			if tc.fellThrough {
				if !strings.Contains(joined, "driver hfsplus matched but failed to open") || !strings.Contains(joined, "opened as fat") {
					t.Errorf("warnings = %q, want the hfsplus failure and \"opened as fat\"", info.Warnings)
				}
			} else if !strings.Contains(joined, "also matched by: hfsplus (ambiguous signatures)") {
				t.Errorf("warnings = %q, want hfsplus named as an ambiguous claimant", info.Warnings)
			}
		})
	}
}

// A real HFS+ volume with a stale FAT boot sector left in its boot blocks
// (bytes 0-1023 are not part of the HFS+ structures) is an HFS+ volume: hfsplus
// precedes fat in the probe order, and its probe depends on its own header
// only. Stale sectors: the one of a real FAT32 volume, ones declaring a size
// between the HFS+ volume and the image (a partition reformatted without
// wiping sector 0), and ones without the jump byte (the FAT driver rejects
// them). Where the FAT driver also matches, the opened filesystem says so.
func TestStaleFATBootSectorDoesNotHideHFSPlus(t *testing.T) {
	boot := gunzipFixture(t, "../fat/testdata/fat32.img.gz")[:512]
	const volume = 256 * 4096
	withTotal := func(sectors uint32, jump bool) []byte {
		b := bytes.Clone(boot)
		binary.LittleEndian.PutUint16(b[19:], 0)
		binary.LittleEndian.PutUint32(b[32:], sectors)
		if !jump {
			b[0] = 0
		}
		return b
	}
	noJump := bytes.Clone(boot)
	noJump[0] = 0
	sectors := map[string][]byte{
		"fat32 fixture sector":             boot,
		"size between volume and image":    withTotal((volume+4096)/512, true),
		"size just under the image":        withTotal((volume+8192-512)/512, true),
		"fixture sector without jump byte": noJump,
		"between volume and image no jump": withTotal((volume+4096)/512, false),
	}
	for name, o := range map[string]hfsplustest.Options{
		"hfsplus": {Label: "H"},
		"hfsx":    {Label: "H", HFSX: true, CaseSensitive: true},
	} {
		typ := name
		for sname, sector := range sectors {
			t.Run(name+"/"+sname, func(t *testing.T) {
				o := o
				o.ImageExtra = 8192
				img := hfsplustest.Build(o, nil)
				copy(img, sector)
				size := int64(len(img))
				r := bytes.NewReader(img)
				if got, ok := detect.Probe(r, size); !ok || got != "hfsplus" {
					t.Fatalf("Probe = %q, %v; want hfsplus", got, ok)
				}
				fsys, err := detect.Open(r, size)
				if err != nil {
					t.Fatal(err)
				}
				info := fsys.Info()
				if info.Type != typ {
					t.Errorf("Info().Type = %q, want %q", info.Type, typ)
				}
				var others []string
				seen := false
				for _, d := range detect.Drivers {
					if d.Name == "hfsplus" {
						seen = true
						continue
					}
					if seen && d.Probe(r, size) {
						others = append(others, d.Name)
					}
				}
				note := ""
				for _, w := range info.Warnings {
					if strings.HasPrefix(w, "also matched by: ") {
						note = w
					}
				}
				want := "also matched by: " + strings.Join(others, ", ") + " (ambiguous signatures)"
				if len(others) > 0 && note != want {
					t.Errorf("note = %q, want %q (warnings %q)", note, want, info.Warnings)
				} else if len(others) == 0 && note != "" {
					t.Errorf("unexpected ambiguity note %q", note)
				}
			})
		}
	}
}

func hfsplusProbe(r io.ReaderAt, size int64) bool {
	for _, d := range detect.Drivers {
		if d.Name == "hfsplus" {
			return d.Probe(r, size)
		}
	}
	return false
}

type stubFS struct {
	filesys.FileSystem
	typ string
}

func (s *stubFS) Info() filesys.Info { return filesys.Info{Type: s.typ} }

// When more than one driver's Probe matches, the opened filesystem names the
// others. Clean images (one claimant, see TestDriversClaimExactlyTheirOwnImages)
// carry no such note, and a driver that matched but failed to open is reported
// by its own note, not as ambiguity.
func TestOpenWithReportsAmbiguousSignatures(t *testing.T) {
	ok := func(name string) detect.Driver {
		return detect.Driver{
			Name:  name,
			Probe: func(io.ReaderAt, int64) bool { return true },
			Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return &stubFS{typ: name}, nil },
		}
	}
	corrupt := detect.Driver{
		Name:  "bad",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) {
			return nil, &filesys.CorruptError{Structure: "bad", Offset: 1, Reason: "no"}
		},
	}
	no := detect.Driver{
		Name:  "no",
		Probe: func(io.ReaderAt, int64) bool { return false },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return &stubFS{typ: "no"}, nil },
	}
	panics := detect.Driver{
		Name:  "panics",
		Probe: func(io.ReaderAt, int64) bool { panic("probe") },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return &stubFS{typ: "panics"}, nil },
	}
	lastResort := ok("fallback")
	lastResort.LastResort = true
	r := bytes.NewReader(make([]byte, 16))
	for name, tc := range map[string]struct {
		drivers []detect.Driver
		typ     string
		warns   []string
	}{
		"single":                    {[]detect.Driver{ok("a"), no}, "a", nil},
		"two match":                 {[]detect.Driver{ok("a"), ok("b")}, "a", []string{"also matched by: b (ambiguous signatures)"}},
		"three match":               {[]detect.Driver{ok("a"), ok("b"), no, ok("c")}, "a", []string{"also matched by: b, c (ambiguous signatures)"}},
		"last resort ignored":       {[]detect.Driver{ok("a"), lastResort}, "a", nil},
		"later probe panic ignored": {[]detect.Driver{ok("a"), panics}, "a", nil},
		"failed then open":          {[]detect.Driver{corrupt, ok("b"), ok("c")}, "b", []string{"", "also matched by: c (ambiguous signatures)"}},
	} {
		t.Run(name, func(t *testing.T) {
			fsys, err := detect.OpenWith(tc.drivers, r, 16)
			if err != nil {
				t.Fatal(err)
			}
			got := fsys.Info().Warnings
			if fsys.Info().Type != tc.typ || len(got) != len(tc.warns) {
				t.Fatalf("type %q warnings %q, want %q %q", fsys.Info().Type, got, tc.typ, tc.warns)
			}
			for i, w := range tc.warns {
				if w == "" { // the fall-through note, worded by OpenWith's existing contract
					if !strings.Contains(got[i], "driver bad matched but failed to open") || !strings.HasSuffix(got[i], "opened as b") {
						t.Errorf("warning %d = %q", i, got[i])
					}
				} else if got[i] != w {
					t.Errorf("warning %d = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

type snapStub struct{ stubFS }

func (*snapStub) SnapshotPath(p, snapshot string) (string, error) { return p + "@" + snapshot, nil }

type viewStub struct{ stubFS }

func (*viewStub) EntrySnapshot(filesys.Entry) (string, uint64, bool) { return "snap", 7, true }

type bothStub struct{ stubFS }

func (*bothStub) SnapshotPath(p, snapshot string) (string, error)    { return p + "@" + snapshot, nil }
func (*bothStub) EntrySnapshot(filesys.Entry) (string, uint64, bool) { return "snap", 7, true }

// A filesystem that carries notes (here an ambiguity note) keeps the optional
// interfaces of the filesystem it wraps (Snapshotter, SnapshotViewer), and gains
// none it did not have.
func TestWarnedFilesystemKeepsOptionalInterfaces(t *testing.T) {
	r := bytes.NewReader(make([]byte, 16))
	other := detect.Driver{
		Name:  "other",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return &stubFS{typ: "other"}, nil },
	}
	for name, tc := range map[string]struct {
		fs                 filesys.FileSystem
		wantSnap, wantView bool
	}{
		"neither":     {&stubFS{typ: "x"}, false, false},
		"snapshotter": {&snapStub{stubFS{typ: "x"}}, true, false},
		"viewer":      {&viewStub{stubFS{typ: "x"}}, false, true},
		"both":        {&bothStub{stubFS{typ: "x"}}, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			first := detect.Driver{
				Name:  "x",
				Probe: func(io.ReaderAt, int64) bool { return true },
				Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return tc.fs, nil },
			}
			fsys, err := detect.OpenWith([]detect.Driver{first, other}, r, 16)
			if err != nil {
				t.Fatal(err)
			}
			if len(fsys.Info().Warnings) == 0 {
				t.Fatal("no note attached: the test does not exercise the wrapper")
			}
			sn, isSnap := fsys.(filesys.Snapshotter)
			sv, isView := fsys.(filesys.SnapshotViewer)
			if isSnap != tc.wantSnap || isView != tc.wantView {
				t.Fatalf("Snapshotter=%v SnapshotViewer=%v, want %v %v", isSnap, isView, tc.wantSnap, tc.wantView)
			}
			if isSnap {
				if got, err := sn.SnapshotPath("/V/a", "s"); err != nil || got != "/V/a@s" {
					t.Errorf("SnapshotPath = %q, %v", got, err)
				}
			}
			if isView {
				if name, xid, ok := sv.EntrySnapshot(filesys.Entry{}); !ok || name != "snap" || xid != 7 {
					t.Errorf("EntrySnapshot = %q %d %v", name, xid, ok)
				}
			}
		})
	}
}

type recStub struct{ stubFS }

func (*recStub) Recoverable(filesys.Entry) ([]filesys.Candidate, error) {
	return []filesys.Candidate{{Method: "stub-method"}}, nil
}

type recSnapStub struct{ snapStub }

func (*recSnapStub) Recoverable(filesys.Entry) ([]filesys.Candidate, error) {
	return []filesys.Candidate{{Method: "stub-method"}}, nil
}

type recViewStub struct{ viewStub }

func (*recViewStub) Recoverable(filesys.Entry) ([]filesys.Candidate, error) {
	return []filesys.Candidate{{Method: "stub-method"}}, nil
}

type recBothStub struct{ bothStub }

func (*recBothStub) Recoverable(filesys.Entry) ([]filesys.Candidate, error) {
	return []filesys.Candidate{{Method: "stub-method"}}, nil
}

// Every note wrapper (plain, snapshotter, viewer, both) forwards Underlying, so filesys.As finds the
// Recoverer behind the note and the wrapper itself offers none (C50).
func TestWarnedWrappersForwardUnderlying(t *testing.T) {
	r := bytes.NewReader(make([]byte, 16))
	other := detect.Driver{
		Name:  "other",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return &stubFS{typ: "other"}, nil },
	}
	for name, inner := range map[string]filesys.FileSystem{
		"plain":       &recStub{stubFS{typ: "x"}},
		"snapshotter": &recSnapStub{snapStub{stubFS{typ: "x"}}},
		"viewer":      &recViewStub{viewStub{stubFS{typ: "x"}}},
		"both":        &recBothStub{bothStub{stubFS{typ: "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			first := detect.Driver{
				Name:  "x",
				Probe: func(io.ReaderAt, int64) bool { return true },
				Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return inner, nil },
			}
			fsys, err := detect.OpenWith([]detect.Driver{first, other}, r, 16)
			if err != nil {
				t.Fatal(err)
			}
			if len(fsys.Info().Warnings) == 0 {
				t.Fatal("no note attached: the test does not exercise the wrapper")
			}
			w, ok := fsys.(filesys.Wrapper)
			if !ok {
				t.Fatalf("%T is not a filesys.Wrapper", fsys)
			}
			if w.Underlying() != inner {
				t.Errorf("Underlying() = %T, want the wrapped %T", w.Underlying(), inner)
			}
			if _, isRec := fsys.(filesys.Recoverer); isRec {
				t.Error("the note wrapper itself implements Recoverer: it must not invent one")
			}
			rec, ok := filesys.As[filesys.Recoverer](fsys)
			if !ok {
				t.Fatal("filesys.As does not find the Recoverer behind the note")
			}
			if cs, err := rec.Recoverable(filesys.Entry{}); err != nil || len(cs) != 1 || cs[0].Method != "stub-method" {
				t.Errorf("Recoverable = %v, %v", cs, err)
			}
		})
	}
}
