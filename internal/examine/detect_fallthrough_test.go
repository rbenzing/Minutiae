package examine_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// onlyPartition returns the info of the single partition of s.
func onlyPartition(t *testing.T, s *examine.Session) examine.PartitionInfo {
	t.Helper()
	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d, want 1", len(info.Partitions))
	}
	return info.Partitions[0]
}

// A stale or forged F2FS backup superblock inside a FAT volume must not hide
// it: the partition is FAT, with no note, and its files are reachable.
func TestInfoStaleF2FSBackupDoesNotHideFAT(t *testing.T) {
	img := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("fat")}})
	binary.LittleEndian.PutUint32(img[4096+1024:], 0xF2F52010)
	binary.LittleEndian.PutUint16(img[4096+1024+4:], 1)
	binary.LittleEndian.PutUint32(img[4096+1024+16:], 12)
	if !detect.Drivers[len(detect.Drivers)-1].Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatal("the planted backup superblock is not seen by the last-resort driver")
	}

	s, _ := imageSession(t, img)
	p := onlyPartition(t, s)
	if p.FSType != "fat16" || p.FSInfo == nil || p.Error != "" {
		t.Fatalf("partition = %+v, want a clean fat16", p)
	}
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Lookup("/A.TXT"); err != nil {
		t.Errorf("Lookup = %v", err)
	}
}

// A destroyed F2FS primary superblock is recovered through the last-resort
// f2fs-backup driver: the partition is f2fs (the type, not the driver name), the
// backup-in-use warning is shown and there is no failure note.
func TestInfoF2FSDestroyedPrimaryOpensThroughBackup(t *testing.T) {
	img := f2fstest.Build(f2fstest.Options{Segments: 1}, []f2fstest.File{{Path: "/a.txt", Data: []byte("a"), Inline: true}})
	clear(img[1024:4096])
	s, _ := imageSession(t, img)
	p := onlyPartition(t, s)
	if p.FSType != "f2fs" || p.FSInfo == nil || p.Error != "" {
		t.Fatalf("partition = %+v, want f2fs", p)
	}
	if !slices.ContainsFunc(p.FSInfo.Warnings, func(w string) bool { return strings.Contains(w, "backup superblock") }) {
		t.Errorf("Warnings = %q, want the backup-superblock warning", p.FSInfo.Warnings)
	}
}

// A driver that matches but fails to open as corrupt does not end the search:
// here a FAT volume carrying a stale ext magic (ext4 probes true, its Open
// fails). The partition is FAT, the note names the failed driver and the
// result, and the files are reachable.
func TestInfoFallsThroughCorruptOpenToLaterDriver(t *testing.T) {
	img := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("fat")}})
	binary.LittleEndian.PutUint16(img[1024+0x38:], 0xEF53)

	s, _ := imageSession(t, img)
	p := onlyPartition(t, s)
	if p.FSType != "fat16" || p.FSInfo == nil {
		t.Fatalf("partition = %+v, want fat16", p)
	}
	if !strings.Contains(p.Error, "driver ext4 matched but failed to open") || !strings.Contains(p.Error, "opened as fat") {
		t.Errorf("Error = %q, want the ext4 failure and \"opened as fat\"", p.Error)
	}
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Lookup("/A.TXT"); err != nil {
		t.Errorf("Lookup = %v", err)
	}
}

func corruptDriver(name string) detect.Driver {
	return detect.Driver{
		Name:  name,
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) {
			return nil, &filesys.CorruptError{Structure: name, Offset: 1, Reason: "bad " + name}
		},
	}
}

// When every matching driver fails the first failure is the partition's error
// (FSType is that driver) and the others are notes; a non-corrupt error from a
// later driver is final and the earlier failure stays a note.
func TestInfoCorruptOpensAllFailAndNonCorruptIsFinal(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(make([]byte, 2048)), 1)
	run := func(drivers ...detect.Driver) (examine.PartitionInfo, error) {
		t.Helper()
		s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: drivers})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		p := onlyPartition(t, s)
		_, _, ferr := s.FS(-1)
		return p, ferr
	}

	p, err := run(corruptDriver("first"), corruptDriver("second"))
	if p.FSType != "first" || p.FSInfo != nil || !strings.Contains(p.Error, "bad first") || !strings.Contains(p.Error, "driver second matched but failed to open") {
		t.Errorf("partition = %+v", p)
	}
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || ce.Structure != "first" {
		t.Errorf("FS(-1) err = %v, want the first driver's CorruptError", err)
	}

	boom := errors.New("boom")
	ioFail := detect.Driver{Name: "iofail", Probe: func(io.ReaderAt, int64) bool { return true }, Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) { return nil, boom }}
	p, err = run(corruptDriver("first"), ioFail, detect.Driver{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open})
	if p.FSType != "iofail" || !strings.Contains(p.Error, "boom") || !strings.Contains(p.Error, "driver first matched but failed to open") || !errors.Is(err, boom) {
		t.Errorf("partition = %+v, err = %v; want the later non-corrupt error final, the first failure kept as a note", p, err)
	}
}
