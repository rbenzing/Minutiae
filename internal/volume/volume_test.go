package volume_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/volume"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

const (
	guidLinux = "0fc63daf-8483-4772-8e79-3d69d8477de4"
	guidEFI   = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	guidA     = "11223344-5566-7788-99aa-bbccddeeff00"
	guidB     = "01234567-89ab-cdef-0123-456789abcdef"
	diskGUID  = "aabbccdd-eeff-0011-2233-445566778899"
)

func read(t *testing.T, img []byte, ss int) *volume.Table {
	t.Helper()
	tab, err := volume.Read(bytes.NewReader(img), int64(len(img)), ss)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return tab
}

func hasWarning(tab *volume.Table, sub string) bool {
	for _, w := range tab.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// fixHeaderCRC recomputes the CRC of the GPT header at byte offset off.
func fixHeaderCRC(img []byte, off int) {
	hs := int(binary.LittleEndian.Uint32(img[off+12:]))
	binary.LittleEndian.PutUint32(img[off+16:], 0)
	binary.LittleEndian.PutUint32(img[off+16:], crc32.ChecksumIEEE(img[off:off+hs]))
}

func TestReadMBRPrimaryAndLogical(t *testing.T) {
	img := volumetest.MBR(16384, []volumetest.Part{
		{StartLBA: 2048, Sectors: 2048, MBRType: 0x83},
		{StartLBA: 4096, Sectors: 1000, MBRType: 0x0c},
		{StartLBA: 8192, Sectors: 1000, MBRType: 0x83, Logical: true},
		{StartLBA: 10000, Sectors: 500, MBRType: 0x07, Logical: true},
	})
	tab := read(t, img, 0)
	if tab.Scheme != "mbr" || tab.SectorSize != 512 {
		t.Fatalf("scheme %q sector %d", tab.Scheme, tab.SectorSize)
	}
	want := []volume.Partition{
		{Index: 1, Start: 2048 * 512, Length: 2048 * 512, Type: "0x83", TypeName: "Linux"},
		{Index: 2, Start: 4096 * 512, Length: 1000 * 512, Type: "0x0c", TypeName: "FAT32"},
		{Index: 5, Start: 8192 * 512, Length: 1000 * 512, Type: "0x83", TypeName: "Linux"},
		{Index: 6, Start: 10000 * 512, Length: 500 * 512, Type: "0x07", TypeName: "NTFS/exFAT"},
	}
	if len(tab.Partitions) != len(want) {
		t.Fatalf("got %d partitions: %+v", len(tab.Partitions), tab.Partitions)
	}
	for i, w := range want {
		if tab.Partitions[i] != w {
			t.Errorf("partition %d: got %+v want %+v", i, tab.Partitions[i], w)
		}
	}
	if len(tab.Warnings) != 1 || tab.Warnings[0] != "MBR sector size assumed to be 512 bytes" {
		t.Errorf("warnings: %v", tab.Warnings)
	}
}

func gptParts() []volumetest.Part {
	return []volumetest.Part{
		{StartLBA: 64, Sectors: 64, TypeGUID: guidEFI, GUID: guidA, Name: "boot"},
		{StartLBA: 128, Sectors: 128, TypeGUID: guidLinux, GUID: guidB, Name: "sysém"},
	}
}

func checkGPT(t *testing.T, tab *volume.Table, ss int) {
	t.Helper()
	if tab.Scheme != "gpt" || tab.SectorSize != ss || tab.DiskGUID != diskGUID {
		t.Fatalf("scheme %q sector %d disk %q", tab.Scheme, tab.SectorSize, tab.DiskGUID)
	}
	s := int64(ss)
	want := []volume.Partition{
		{Index: 1, Start: 64 * s, Length: 64 * s, Type: guidEFI, TypeName: "EFI System", Name: "boot", GUID: guidA},
		{Index: 2, Start: 128 * s, Length: 128 * s, Type: guidLinux, TypeName: "Linux filesystem", Name: "sysém", GUID: guidB},
	}
	if len(tab.Partitions) != len(want) {
		t.Fatalf("got %d partitions: %+v", len(tab.Partitions), tab.Partitions)
	}
	for i, w := range want {
		if tab.Partitions[i] != w {
			t.Errorf("partition %d: got %+v want %+v", i, tab.Partitions[i], w)
		}
	}
	if len(tab.Warnings) != 0 {
		t.Errorf("warnings: %v", tab.Warnings)
	}
}

func TestReadGPT512(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, gptParts())
	checkGPT(t, read(t, img, 0), 512)
	checkGPT(t, read(t, img, 512), 512)
}

func TestReadGPT4096(t *testing.T) {
	img := volumetest.GPT(4096, 2048, diskGUID, gptParts())
	checkGPT(t, read(t, img, 0), 4096) // probing
	checkGPT(t, read(t, img, 4096), 4096)
}

func TestReadGPTBackupFallback(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, gptParts())
	img[512+16] ^= 0xff // primary header CRC
	tab := read(t, img, 0)
	if tab.Scheme != "gpt" || len(tab.Partitions) != 2 {
		t.Fatalf("scheme %q partitions %+v", tab.Scheme, tab.Partitions)
	}
	if !hasWarning(tab, "primary GPT header invalid; using backup") {
		t.Errorf("warnings: %v", tab.Warnings)
	}
	if tab.Partitions[0].Name != "boot" || tab.DiskGUID != diskGUID {
		t.Errorf("unexpected data from backup: %+v %q", tab.Partitions[0], tab.DiskGUID)
	}
}

func TestReadGPTBackupFallback4096(t *testing.T) {
	img := volumetest.GPT(4096, 2048, diskGUID, gptParts())
	img[4096+16] ^= 0xff
	tab := read(t, img, 0)
	if tab.Scheme != "gpt" || tab.SectorSize != 4096 || len(tab.Partitions) != 2 || !hasWarning(tab, "backup") {
		t.Fatalf("got %+v", tab)
	}
}

func TestReadGPTHugeEntryCount(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, gptParts())
	binary.LittleEndian.PutUint32(img[512+80:], 0xFFFFFFFF)
	fixHeaderCRC(img, 512)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tab, err := volume.Read(bytes.NewReader(img), int64(len(img)), 0)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("Read allocated %d bytes", alloc)
	}
	if tab.Scheme != "gpt" || len(tab.Partitions) != 2 || !hasWarning(tab, "backup") {
		t.Errorf("expected backup fallback, got %+v", tab)
	}

	// Both headers hostile: no GPT, protective MBR only; still bounded.
	last := (len(img)/512 - 1) * 512
	binary.LittleEndian.PutUint32(img[last+80:], 0xFFFFFFFF)
	fixHeaderCRC(img, last)
	runtime.ReadMemStats(&before)
	tab = read(t, img, 0)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("Read allocated %d bytes", alloc)
	}
	if tab.Scheme != "mbr" || len(tab.Partitions) != 1 || tab.Partitions[0].Type != "0xee" ||
		tab.Partitions[0].TypeName != "GPT protective" {
		t.Fatalf("expected protective MBR, got %+v", tab)
	}
	if !hasWarning(tab, "no valid GPT") || !hasWarning(tab, "invalid entry count") {
		t.Errorf("warnings: %v", tab.Warnings)
	}
}

func TestReadGPTRejectsHostileHeaderFields(t *testing.T) {
	for name, mutate := range map[string]func(img []byte){
		"entry size 130":     func(img []byte) { binary.LittleEndian.PutUint32(img[512+84:], 130) },
		"entry size 64":      func(img []byte) { binary.LittleEndian.PutUint32(img[512+84:], 64) },
		"entry size 8192":    func(img []byte) { binary.LittleEndian.PutUint32(img[512+84:], 8192) },
		"count 1025":         func(img []byte) { binary.LittleEndian.PutUint32(img[512+80:], 1025) },
		"array outside":      func(img []byte) { binary.LittleEndian.PutUint64(img[512+72:], 1<<40) },
		"array lba overflow": func(img []byte) { binary.LittleEndian.PutUint64(img[512+72:], ^uint64(0)) },
		"header size 91":     func(img []byte) { binary.LittleEndian.PutUint32(img[512+12:], 91) },
		"header size 513":    func(img []byte) { binary.LittleEndian.PutUint32(img[512+12:], 513) },
	} {
		t.Run(name, func(t *testing.T) {
			img := volumetest.GPT(512, 16384, diskGUID, gptParts())
			mutate(img)
			if !strings.HasPrefix(name, "header size") {
				fixHeaderCRC(img, 512)
			}
			tab := read(t, img, 0)
			if !hasWarning(tab, "primary GPT header invalid; using backup") || len(tab.Partitions) != 2 {
				t.Errorf("expected backup fallback, got %+v", tab)
			}
		})
	}
}

func TestReadGPTEntryArrayCRC(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, gptParts())
	img[2*512+56] ^= 0x01 // corrupt a name byte in the primary array only
	tab := read(t, img, 0)
	if !hasWarning(tab, "using backup") || tab.Partitions[0].Name != "boot" {
		t.Errorf("got %+v", tab)
	}
}

func TestReadMBRExtendedLoop(t *testing.T) {
	parts := []volumetest.Part{
		{StartLBA: 8192, Sectors: 1000, MBRType: 0x83, Logical: true},
		{StartLBA: 10000, Sectors: 500, MBRType: 0x83, Logical: true},
	}
	t.Run("second EBR links to itself", func(t *testing.T) {
		img := volumetest.MBR(16384, parts)
		const extStart, ebr2 = 8191, 9999
		binary.LittleEndian.PutUint32(img[ebr2*512+462+8:], ebr2-extStart)
		img[ebr2*512+462+4] = 0x0f
		img[ebr2*512+462+12] = 1
		tab := read(t, img, 0)
		if !hasWarning(tab, "loop") || len(tab.Partitions) != 2 {
			t.Errorf("got %d partitions, warnings %v", len(tab.Partitions), tab.Warnings)
		}
	})
	t.Run("first EBR links to itself", func(t *testing.T) {
		img := volumetest.MBR(16384, parts)
		const ebr1 = 8191
		binary.LittleEndian.PutUint32(img[ebr1*512+462+8:], 0)
		tab := read(t, img, 0)
		if !hasWarning(tab, "loop") || len(tab.Partitions) != 1 {
			t.Errorf("got %d partitions, warnings %v", len(tab.Partitions), tab.Warnings)
		}
	})
}

func TestReadMBRLogicalCap(t *testing.T) {
	var parts []volumetest.Part
	for i := range 130 {
		parts = append(parts, volumetest.Part{StartLBA: uint64(10 * (i + 2)), Sectors: 5, MBRType: 0x83, Logical: true})
	}
	tab := read(t, volumetest.MBR(2000, parts), 0)
	if len(tab.Partitions) != 128 || tab.Partitions[127].Index != 132 || !hasWarning(tab, "more than 128") {
		t.Errorf("got %d partitions, warnings %v", len(tab.Partitions), tab.Warnings)
	}
}

func fat32BootSector() []byte {
	s := make([]byte, 512)
	s[0], s[1], s[2] = 0xEB, 0x58, 0x90
	copy(s[3:], "MSDOS5.0")
	binary.LittleEndian.PutUint16(s[11:], 512)
	s[13] = 8
	binary.LittleEndian.PutUint16(s[14:], 32)
	s[16] = 2
	s[510], s[511] = 0x55, 0xAA
	return s
}

func TestReadBootSectorIsNotMBR(t *testing.T) {
	img := make([]byte, 1<<20)
	copy(img, fat32BootSector())
	// Bytes in the partition-entry area that would otherwise look like an entry.
	img[446+4] = 0x0c
	binary.LittleEndian.PutUint32(img[446+8:], 1)
	binary.LittleEndian.PutUint32(img[446+12:], 100)
	tab := read(t, img, 0)
	if tab.Scheme != "none" {
		t.Fatalf("scheme %q", tab.Scheme)
	}
}

func TestLooksLikeBootSector(t *testing.T) {
	ntfs := make([]byte, 512)
	ntfs[0], ntfs[1], ntfs[2] = 0xEB, 0x52, 0x90
	copy(ntfs[3:], "NTFS    ")
	exfat := make([]byte, 512)
	exfat[0], exfat[1], exfat[2] = 0xEB, 0x76, 0x90
	copy(exfat[3:], "EXFAT   ")
	jmpNear := fat32BootSector()
	jmpNear[0], jmpNear[1], jmpNear[2] = 0xE9, 0x00, 0x00
	bootloader := make([]byte, 512)
	bootloader[0], bootloader[1], bootloader[2] = 0xEB, 0x63, 0x90
	badSPC := fat32BootSector()
	badSPC[13] = 3
	noReserved := fat32BootSector()
	binary.LittleEndian.PutUint16(noReserved[14:], 0)
	threeFATs := fat32BootSector()
	threeFATs[16] = 3
	noJump := fat32BootSector()
	noJump[0] = 0x33
	for name, tc := range map[string]struct {
		in   []byte
		want bool
	}{
		"fat32":       {fat32BootSector(), true},
		"ntfs":        {ntfs, true},
		"exfat":       {exfat, true},
		"fat jmp e9":  {jmpNear, true},
		"bootloader":  {bootloader, false},
		"bad spc":     {badSPC, false},
		"no reserved": {noReserved, false},
		"three fats":  {threeFATs, false},
		"no jump":     {noJump, false},
		"short":       {[]byte{0xEB, 0x58, 0x90}, false},
		"nil":         {nil, false},
	} {
		if got := volume.LooksLikeBootSector(tc.in); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

func TestReadNoTable(t *testing.T) {
	img := make([]byte, 1<<20)
	tab := read(t, img, 0)
	if tab.Scheme != "none" || len(tab.Partitions) != 1 || len(tab.Unallocated) != 0 {
		t.Fatalf("got %+v", tab)
	}
	want := volume.Partition{Index: 0, Start: 0, Length: 1 << 20, TypeName: "whole image"}
	if tab.Partitions[0] != want {
		t.Errorf("got %+v", tab.Partitions[0])
	}
	// Tiny and empty images are fine too.
	for _, n := range []int{0, 1, 511} {
		tab := read(t, make([]byte, n), 0)
		if tab.Scheme != "none" || tab.Partitions[0].Length != int64(n) {
			t.Errorf("size %d: %+v", n, tab)
		}
	}
}

func TestReadInvalidArguments(t *testing.T) {
	if _, err := volume.Read(bytes.NewReader(nil), 0, 1000); err == nil {
		t.Error("sector size 1000 accepted")
	}
	if _, err := volume.Read(bytes.NewReader(nil), -1, 0); err == nil {
		t.Error("negative size accepted")
	}
	if _, err := volume.Read(nil, 0, 0); err == nil {
		t.Error("nil reader accepted")
	}
}

func TestReadTruncatedPartitionClamped(t *testing.T) {
	parts := append(gptParts(), volumetest.Part{StartLBA: 12000, Sectors: 100, TypeGUID: guidLinux, GUID: guidA, Name: "late"})
	img := volumetest.GPT(512, 16384, diskGUID, parts)
	img = img[:200*512+100] // ends inside partition 2 (sectors 128..255), mid-sector
	tab := read(t, img, 0)
	if tab.Scheme != "gpt" || len(tab.Partitions) != 3 {
		t.Fatalf("scheme %q partitions %+v", tab.Scheme, tab.Partitions)
	}
	p := tab.Partitions[1]
	if p.Start != 128*512 || p.Start+p.Length != int64(len(img)) {
		t.Errorf("partition 2 not clamped to image end: %+v (image %d)", p, len(img))
	}
	if !hasWarning(tab, "partition 2 extends past end of image (truncated image)") {
		t.Errorf("warnings: %v", tab.Warnings)
	}
	if q := tab.Partitions[2]; q.Index != 3 || q.Start != int64(len(img)) || q.Length != 0 || q.Name != "late" {
		t.Errorf("partition wholly past the end must stay listed with length 0: %+v", q)
	}
	if !hasWarning(tab, "partition 3 starts past end of image (truncated image)") {
		t.Errorf("warnings: %v", tab.Warnings)
	}
	for _, u := range tab.Unallocated {
		if u.Offset < 0 || u.Offset+u.Length > int64(len(img)) {
			t.Errorf("unallocated run out of image: %+v", u)
		}
	}
}

func TestReadTruncatedMBRPartitionClamped(t *testing.T) {
	img := volumetest.MBR(16384, []volumetest.Part{{StartLBA: 2048, Sectors: 4096, MBRType: 0x83}})
	img = img[:3000*512]
	tab := read(t, img, 0)
	if len(tab.Partitions) != 1 || tab.Partitions[0].Length != (3000-2048)*512 ||
		!hasWarning(tab, "partition 1 extends past end of image (truncated image)") {
		t.Errorf("got %+v", tab)
	}
}

// sectors builds runs from [start,end) sector pairs.
func sectors(n ...int64) []volume.Run {
	var out []volume.Run
	for i := 0; i+1 < len(n); i += 2 {
		out = append(out, volume.Run{Offset: n[i] * 512, Length: (n[i+1] - n[i]) * 512})
	}
	return out
}

func equalRuns(a, b []volume.Run) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUnallocatedGaps(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, []volumetest.Part{
		{StartLBA: 2048, Sectors: 2048, TypeGUID: guidLinux, GUID: guidA, Name: "a"},
		{StartLBA: 8192, Sectors: 2048, TypeGUID: guidLinux, GUID: guidB, Name: "b"},
	})
	tab := read(t, img, 0)
	// Sectors 0-33 are the MBR, header and primary entries; 16351-16382 the
	// backup entries and 16383 the backup header.
	want := sectors(34, 2048, 4096, 8192, 10240, 16351)
	if !equalRuns(tab.Unallocated, want) {
		t.Errorf("got  %v\nwant %v", tab.Unallocated, want)
	}
}

func TestUnallocatedMBR(t *testing.T) {
	img := volumetest.MBR(16384, []volumetest.Part{
		{StartLBA: 2048, Sectors: 2048, MBRType: 0x83},
		{StartLBA: 8192, Sectors: 1000, MBRType: 0x83, Logical: true},
		{StartLBA: 10000, Sectors: 500, MBRType: 0x83, Logical: true},
	})
	tab := read(t, img, 0)
	// EBRs sit at 8191 and 9999; the extended container itself is not a
	// partition, so its slack counts as unallocated.
	want := sectors(1, 2048, 4096, 8191, 9192, 9999, 10500, 16384)
	if !equalRuns(tab.Unallocated, want) {
		t.Errorf("got  %v\nwant %v", tab.Unallocated, want)
	}
}

func TestUnallocatedBackupFallback(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, []volumetest.Part{
		{StartLBA: 2048, Sectors: 2048, TypeGUID: guidLinux, GUID: guidA, Name: "a"},
	})
	img[512+16] ^= 0xff
	tab := read(t, img, 0)
	// The primary array location is not trusted when the primary is invalid,
	// so only sectors 0-1 and the backup structures are excluded.
	want := sectors(2, 2048, 4096, 16351)
	if !equalRuns(tab.Unallocated, want) {
		t.Errorf("got  %v\nwant %v", tab.Unallocated, want)
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("device gone") }

func TestReadPropagatesIOError(t *testing.T) {
	if _, err := volume.Read(failingReader{}, 1<<20, 0); err == nil {
		t.Error("I/O error swallowed")
	}
}

func TestReadMBRPartitionWhollyPastEndListed(t *testing.T) {
	img := volumetest.MBR(16384, []volumetest.Part{
		{StartLBA: 2048, Sectors: 100, MBRType: 0x83},
		{StartLBA: 9600, Sectors: 100, MBRType: 0x83},
		{StartLBA: 9500, Sectors: 100, MBRType: 0x83, Logical: true},
	})
	img = img[:9500*512] // the EBR (sector 9499) is inside, its logical partition is not
	tab := read(t, img, 512)
	if len(tab.Partitions) != 3 {
		t.Fatalf("got %+v", tab.Partitions)
	}
	for i, idx := range []int{1, 2, 5} {
		p := tab.Partitions[i]
		wantLen := int64(0)
		if idx == 1 {
			wantLen = 100 * 512
		}
		if p.Index != idx || p.Length != wantLen || p.Start+p.Length > int64(len(img)) || p.Start < 0 {
			t.Errorf("partition %d: %+v", idx, p)
		}
		if idx != 1 && !hasWarning(tab, "partition "+string(rune('0'+idx))+" starts past end of image (truncated image)") {
			t.Errorf("no warning for %d: %v", idx, tab.Warnings)
		}
	}
}

func TestReadMBREBRLinkMustBeExtendedType(t *testing.T) {
	img := volumetest.MBR(16384, []volumetest.Part{
		{StartLBA: 8192, Sectors: 1000, MBRType: 0x83, Logical: true},
		{StartLBA: 10000, Sectors: 500, MBRType: 0x83, Logical: true},
	})
	// Turn the first EBR's link entry into a non-extended entry (garbage).
	img[8191*512+462+4] = 0x83
	tab := read(t, img, 512)
	if len(tab.Partitions) != 1 || tab.Partitions[0].Index != 5 {
		t.Errorf("followed a non-extended link: %+v", tab.Partitions)
	}
	// Extended type with a zero sector count is not followed either.
	img = volumetest.MBR(16384, []volumetest.Part{
		{StartLBA: 8192, Sectors: 1000, MBRType: 0x83, Logical: true},
		{StartLBA: 10000, Sectors: 500, MBRType: 0x83, Logical: true},
	})
	binary.LittleEndian.PutUint32(img[8191*512+462+12:], 0)
	if tab := read(t, img, 512); len(tab.Partitions) != 1 {
		t.Errorf("followed a zero-count link: %+v", tab.Partitions)
	}
}

func TestReadMBRSectorSizeAssumedWarning(t *testing.T) {
	img := volumetest.MBR(4096, []volumetest.Part{{StartLBA: 64, Sectors: 64, MBRType: 0x83}})
	if tab := read(t, img, 0); !hasWarning(tab, "MBR sector size assumed to be 512 bytes") {
		t.Errorf("probing: warnings %v", tab.Warnings)
	}
	if tab := read(t, img, 512); len(tab.Warnings) != 0 {
		t.Errorf("explicit sector size: warnings %v", tab.Warnings)
	}
}

func TestReadGPTPrimaryBackupDiffer(t *testing.T) {
	img := volumetest.GPT(512, 16384, diskGUID, gptParts())
	if tab := read(t, img, 0); hasWarning(tab, "differ") {
		t.Fatalf("unexpected differ warning: %v", tab.Warnings)
	}
	last := (len(img)/512 - 1) * 512
	img[last+56] ^= 0xff // backup disk GUID
	fixHeaderCRC(img, last)
	tab := read(t, img, 0)
	if !hasWarning(tab, "primary and backup GPT differ") || tab.DiskGUID != diskGUID || len(tab.Partitions) != 2 {
		t.Errorf("got %+v", tab)
	}
	// Backup entry array altered (CRCs fixed): also a difference.
	img = volumetest.GPT(512, 16384, diskGUID, gptParts())
	arr := (len(img)/512 - 1 - 32) * 512
	img[arr+56] ^= 0x01
	binary.LittleEndian.PutUint32(img[last+88:], crc32.ChecksumIEEE(img[arr:arr+128*128]))
	fixHeaderCRC(img, last)
	if tab := read(t, img, 0); !hasWarning(tab, "primary and backup GPT differ") {
		t.Errorf("array difference: %v", tab.Warnings)
	}
}

func TestReadMBRWithBootloaderJumpIsMBR(t *testing.T) {
	img := volumetest.MBR(4096, []volumetest.Part{
		{StartLBA: 64, Sectors: 64, MBRType: 0x83},
		{StartLBA: 200, Sectors: 64, MBRType: 0x0c},
	})
	img[0], img[1], img[2] = 0xEB, 0x63, 0x90 // bootloader jump, zero BPB
	if volume.LooksLikeBootSector(img[:512]) {
		t.Fatal("test setup: sector should not look like a boot sector")
	}
	tab := read(t, img, 0)
	if tab.Scheme != "mbr" || len(tab.Partitions) != 2 {
		t.Errorf("got %+v", tab)
	}
}
