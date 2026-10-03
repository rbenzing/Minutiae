package examine_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// TestInfoDetectsExt4Partition runs the default driver registry over a GPT image
// whose partition holds an ext4 filesystem.
func TestInfoDetectsExt4Partition(t *testing.T) {
	c := newCase(t)
	fsImg := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true, Label: "root"},
		[]ext4test.File{{Path: "/hello.txt", Data: []byte("hello")}})
	recs := importImage(t, c, disk(fsImg), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // nil Drivers = detect.Drivers
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d, want 1", len(info.Partitions))
	}
	p := info.Partitions[0]
	if p.FSType != "ext4" || p.FSInfo == nil || p.FSInfo.Type != "ext4" || p.FSInfo.Label != "root" || p.Error != "" {
		t.Errorf("partition = %+v (FSInfo %+v), want an ext4 filesystem labelled root", p, p.FSInfo)
	}
}
