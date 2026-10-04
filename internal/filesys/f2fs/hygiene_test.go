package f2fs_test

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Checkpoint flags (ckpt_flags), header offset 132.
const (
	cpFlagsOff    = 132
	cpValidNodes  = 144
	cpNextFreeNid = 152
	cpUmount      = 0x1
	cpOrphan      = 0x2
	cpError       = 0x8
	cpCRCRecovery = 0x40
	cpDisabled    = 0x1000
)

func TestBlankPackHandling(t *testing.T) {
	const feature = "checkpoint pack 2 blank"
	// A blank pack 2 is what mkfs leaves: silent at any checkpoint version,
	// recorded as an Info feature.
	for _, ver := range []uint64{0, 2, 7, 900} {
		o := smallOpts()
		o.NoPack2 = true
		o.Version = ver
		f := mustOpen(t, f2fstest.Build(o, nil))
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("version %d, pack 2 blank: warnings %q", ver, w)
		}
		if !slices.Contains(f.Info().Features, feature) {
			t.Errorf("version %d: features %q lack %q", ver, f.Info().Features, feature)
		}
	}
	// With both packs written there is no such feature.
	if f := mustOpen(t, f2fstest.Build(smallOpts(), nil)); slices.Contains(f.Info().Features, feature) {
		t.Errorf("features %q", f.Info().Features)
	}

	// A blank pack 1 while pack 2 is valid is never normal, whatever the version.
	for _, ver := range []uint64{2, 7} {
		o := smallOpts()
		o.Version = ver
		o.Pack2Newer = true
		img := f2fstest.Build(o, nil)
		g := f2fstest.Geometry(o)
		clear(img[int(g.CP)*4096 : (int(g.CP)+1)*4096])
		f := mustOpen(t, img)
		if cp := f.Checkpoint(); cp.Pack != 2 {
			t.Errorf("version %d: used pack %d", ver, cp.Pack)
		}
		if !hasWarning(f.Info(), "checkpoint pack 1 is blank; using pack 2 (possible rollback)") {
			t.Errorf("version %d, pack 1 blank: warnings %q", ver, f.Info().Warnings)
		}
		if slices.Contains(f.Info().Features, feature) {
			t.Errorf("version %d: pack 2 is not blank but features are %q", ver, f.Info().Features)
		}
	}
}

func TestCheckpointStateFlagsWarn(t *testing.T) {
	g := f2fstest.Geometry(smallOpts())
	cases := []struct {
		name  string
		flags uint32
		want  string
	}{
		{"not an unmount checkpoint", 0, "last checkpoint is not an unmount checkpoint (data written after it is not reflected)"},
		{"error", cpUmount | cpError, "CP_ERROR_FLAG"},
		{"disabled", cpUmount | cpDisabled, "CP_DISABLED_FLAG"},
		{"orphans", cpUmount | cpOrphan, "CP_ORPHAN_PRESENT_FLAG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := f2fstest.Build(smallOpts(), nil)
			cp32(img, g, cpFlagsOff, tc.flags)
			f := mustOpen(t, img)
			if !hasWarning(f.Info(), tc.want) {
				t.Errorf("flags %#x: warnings %q, want %q", tc.flags, f.Info().Warnings, tc.want)
			}
		})
	}
	// The kernel sets CP_CRC_RECOVERY_FLAG on every checkpoint it writes with
	// fsync data in flight; it says nothing about this checkpoint's own state.
	t.Run("crc recovery flag is silent", func(t *testing.T) {
		img := f2fstest.Build(smallOpts(), nil)
		cp32(img, g, cpFlagsOff, cpUmount|cpCRCRecovery)
		if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
			t.Errorf("warnings %q", w)
		}
	})
	t.Run("clean checkpoint is silent", func(t *testing.T) {
		img := f2fstest.Build(smallOpts(), nil)
		cp32(img, g, cpFlagsOff, cpUmount)
		if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
			t.Errorf("warnings %q", w)
		}
	})
}

func TestCheckpointBitmapDoesNotFit(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)
	g := f2fstest.Geometry(smallOpts())
	// A NAT of 130 segments needs 130/2*512/8 = 4160 bytes of version bitmap,
	// more than a checkpoint block holds beside the SIT bitmap at byte 192.
	const natSegs = 130
	nat := g.NAT
	ssa := nat + natSegs*512
	mainAddr := ssa + 512
	sb32(img, sbSegNAT, natSegs)
	sb32(img, sbSSAAddr, ssa)
	sb32(img, sbMainAddr, mainAddr)
	sb32(img, sbSegmentCount, 2+2+natSegs+1+1)
	sb64(img, sbBlockCount, uint64(mainAddr)+512)
	cp32(img, g, cpNATBitmapBytes, natSegs/2*512/8) // matches the geometry, so only the fit is wrong

	_, err := open(img)
	ce := asCorrupt(t, err)
	if want := "version bitmap (4160 bytes at 256) does not fit"; !bytes.Contains([]byte(ce.Reason), []byte(want)) {
		t.Errorf("reason %q does not contain %q", ce.Reason, want)
	}
}

type ioFault struct {
	r       io.ReaderAt
	failAt  int64 // reads touching [failAt, ∞) fail
	armed   bool
	errFail error
}

func (f *ioFault) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off+int64(len(p)) > f.failAt {
		return 0, f.errFail
	}
	return f.r.ReadAt(p, off)
}

func TestOpenReadError(t *testing.T) {
	injected := errors.New("device unplugged")
	wantIO := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, injected) {
			t.Fatalf("error %v does not wrap the injected I/O error", err)
		}
		if errors.Is(err, filesys.ErrCorrupt) {
			t.Fatalf("an I/O error was reported as corruption: %v", err)
		}
	}
	o := smallOpts()
	g := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)

	t.Run("superblock", func(t *testing.T) {
		r := &ioFault{r: bytes.NewReader(img), failAt: 0, armed: true, errFail: injected}
		f, err := f2fs.Open(r, int64(len(img)))
		if f != nil {
			t.Fatal("Open returned a filesystem")
		}
		wantIO(t, err)
	})
	t.Run("checkpoint", func(t *testing.T) {
		r := &ioFault{r: bytes.NewReader(img), failAt: int64(g.CP) * 4096, armed: true, errFail: injected}
		_, err := f2fs.Open(r, int64(len(img)))
		wantIO(t, err)
	})
	t.Run("second checkpoint pack", func(t *testing.T) {
		r := &ioFault{r: bytes.NewReader(img), failAt: (int64(g.CP) + 512) * 4096, armed: true, errFail: injected}
		_, err := f2fs.Open(r, int64(len(img)))
		wantIO(t, err)
	})
	t.Run("NAT journal and block after Open", func(t *testing.T) {
		oo := o
		oo.Nodes = []f2fstest.Node{{NID: 5, Block: fileInode(o, 5, 1)}}
		img := f2fstest.Build(oo, nil)
		r := &ioFault{r: bytes.NewReader(img), failAt: int64(g.CP) * 4096, errFail: injected}
		f, err := f2fs.Open(r, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		_, err = f.NATLookup(5)
		wantIO(t, err)
		// The failure was transient: the journal is not poisoned.
		r.armed = false
		if _, err := f.NATLookup(5); err != nil {
			t.Fatalf("lookup after the device came back: %v", err)
		}
		// And a failing node block read is an I/O error too (fresh FS: the
		// metadata cache would otherwise serve it).
		f, _ = f2fs.Open(r, int64(len(img)))
		if _, err := f.NATLookup(5); err != nil {
			t.Fatal(err)
		}
		r.failAt, r.armed = int64(g.Main)*4096, true
		_, err = f.Node(5)
		wantIO(t, err)
	})
	t.Run("a truncated image is still corruption", func(t *testing.T) {
		_, err := open(img[:8192])
		_ = asCorrupt(t, err)
	})
}

func TestHygieneGeometry(t *testing.T) {
	g := f2fstest.Geometry(smallOpts())
	img := f2fstest.Build(smallOpts(), nil)

	t.Run("SIT too small for the main area", func(t *testing.T) {
		// 30000 main segments need ceil(30000/55) = 546 SIT blocks per copy; the
		// builder's SIT has 512.
		c := slices.Clone(img)
		const main = 30000
		sb32(c, sbSegMain, main)
		sb32(c, sbSectionCount, main)
		sb32(c, sbSegmentCount, 6+main)
		sb64(c, sbBlockCount, uint64(g.Main)+main*512)
		_, err := open(c)
		if ce := asCorrupt(t, err); !regexp.MustCompile(`SIT area holds 512 blocks per copy but 30000 main-area segments need 546`).MatchString(ce.Reason) {
			t.Errorf("reason %q", ce.Reason)
		}
		// 28160 segments (512 * 55) are exactly enough.
		c = slices.Clone(img)
		const fits = 512 * 55
		sb32(c, sbSegMain, fits)
		sb32(c, sbSectionCount, fits)
		sb32(c, sbSegmentCount, 6+fits)
		sb64(c, sbBlockCount, uint64(g.Main)+fits*512)
		if _, err := open(c); err != nil {
			t.Errorf("exactly enough SIT blocks rejected: %v", err)
		}
	})
	t.Run("NAT too small for the reserved inodes", func(t *testing.T) {
		c := slices.Clone(img)
		sb32(c, sbRootIno, g.NATCapacity()) // one past the last nid
		_, err := open(c)
		if ce := asCorrupt(t, err); !regexp.MustCompile(`cannot hold reserved inode`).MatchString(ce.Reason) {
			t.Errorf("reason %q", ce.Reason)
		}
		c = slices.Clone(img)
		sb32(c, sbRootIno, g.NATCapacity()-1)
		if _, err := open(c); err != nil {
			t.Errorf("the last nid is a legal root_ino: %v", err)
		}
	})
	t.Run("main segments differ from section_count x segs_per_sec", func(t *testing.T) {
		c := slices.Clone(img)
		sb32(c, sbSectionCount, 2)
		f := mustOpen(t, c)
		if !hasWarning(f.Info(), "segment_count_main 1 differs from section_count 2 x segs_per_sec 1") {
			t.Errorf("warnings %q", f.Info().Warnings)
		}
		if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
			t.Errorf("consistent geometry warns: %q", w)
		}
	})
}

// A superblock needs major_ver 1 and 4 KiB blocks to be valid: in the primary
// for Probe, in the backup (with an invalid primary) for ProbeBackup.
func TestProbeRequiresSupportedGeometry(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)[:8192]
	probe := func(b []byte) bool { return f2fs.Probe(bytes.NewReader(b), int64(len(b))) }
	if !probe(img) {
		t.Fatal("a valid image does not probe")
	}
	for _, f := range []struct {
		name string
		off  int
		put  func(b []byte, base int)
	}{
		{"major_ver 2", sbMajorVer, func(b []byte, base int) { le.PutUint16(b[base+sbMajorVer:], 2) }},
		{"major_ver 0", sbMajorVer, func(b []byte, base int) { le.PutUint16(b[base+sbMajorVer:], 0) }},
		{"log_blocksize 13", sbLogBlockSize, func(b []byte, base int) { le.PutUint32(b[base+sbLogBlockSize:], 13) }},
		{"log_blocksize 9", sbLogBlockSize, func(b []byte, base int) { le.PutUint32(b[base+sbLogBlockSize:], 9) }},
	} {
		t.Run(f.name, func(t *testing.T) {
			// Probe looks at the primary only; ProbeBackup is the last resort for a
			// destroyed primary with a supported backup. Two bad copies match neither.
			probeBackup := func(b []byte) bool { return f2fs.ProbeBackup(bytes.NewReader(b), int64(len(b))) }
			one := slices.Clone(img)
			f.put(one, primary)
			if probe(one) || !probeBackup(one) {
				t.Error("with only the primary copy unsupported, Probe must be false and ProbeBackup true")
			}
			onlyBackup := slices.Clone(img)
			f.put(onlyBackup, backup)
			if !probe(onlyBackup) || probeBackup(onlyBackup) {
				t.Error("with only the backup copy unsupported, Probe must be true and ProbeBackup false")
			}
			two := slices.Clone(one)
			f.put(two, backup)
			if probe(two) || probeBackup(two) {
				t.Error("a probe matched although neither superblock copy is supported")
			}
		})
	}
}

func TestChecksumMessagesUseEightHexDigits(t *testing.T) {
	re := regexp.MustCompile(`stored 0x[0-9a-f]{8}, computed 0x[0-9a-f]{8}`)

	o := smallOpts()
	o.SBChksum = true
	img := f2fstest.Build(o, nil)
	img[primary+200] ^= 1
	img[backup+200] ^= 1
	_, err := open(img)
	if ce := asCorrupt(t, err); !re.MatchString(ce.Reason) {
		t.Errorf("superblock reason %q", ce.Reason)
	}

	g := f2fstest.Geometry(smallOpts())
	img = f2fstest.Build(smallOpts(), nil)
	cpBlock(img, g, 1)[cpValidBlocks]++
	f := mustOpen(t, img) // pack 2 takes over
	found := false
	for _, w := range f.Info().Warnings {
		found = found || re.MatchString(w)
	}
	if !found {
		t.Errorf("checkpoint warnings %q", f.Info().Warnings)
	}
}

func TestCheckpointCountsBeyondNATCapacityWarn(t *testing.T) {
	o := smallOpts()
	g := f2fstest.Geometry(o)
	capacity := g.NATCapacity()
	for _, tc := range []struct {
		name string
		off  int
		val  uint32
		warn bool
	}{
		{"next_free_nid at capacity", cpNextFreeNid, capacity, false},
		{"next_free_nid beyond capacity", cpNextFreeNid, capacity + 1, true},
		{"valid_node_count beyond capacity", cpValidNodes, capacity + 1, true},
		{"valid_node_count at capacity", cpValidNodes, capacity, false},
		{"next_free_nid 0xFFFFFFFF", cpNextFreeNid, 0xFFFFFFFF, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := f2fstest.Build(o, nil)
			cp32(img, g, tc.off, tc.val)
			f := mustOpen(t, img)
			if got := hasWarning(f.Info(), "exceeds the NAT capacity"); got != tc.warn {
				t.Errorf("warned=%v, want %v: %q", got, tc.warn, f.Info().Warnings)
			}
		})
	}
}
