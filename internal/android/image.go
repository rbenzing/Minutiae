package android

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/device"
)

var (
	partNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	blockPathRe = regexp.MustCompile(`^/dev/block/[A-Za-z0-9_./-]+$`)
)

// validBlockPath accepts only canonical paths under /dev/block: no "." or
// ".." segments and no empty segments, so a hostile by-name link or
// /proc/partitions entry cannot point dd anywhere else.
func validBlockPath(p string) bool {
	if !blockPathRe.MatchString(p) || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// suWrappers are tried in order: Magisk/SuperSU style, then AOSP su. exec:
// runs the command through the device's sh -c, so the trailing redirect
// discards su's own stderr (warnings, SELinux notices) before it can be
// interleaved with the command's stdout — for dd, the image stream.
var suWrappers = []func(string) string{
	func(cmd string) string { return "su -c '" + cmd + "' 2>/dev/null" },
	func(cmd string) string { return "su 0 sh -c '" + cmd + "' 2>/dev/null" },
}

func (d *Device) rootWrapper(ctx context.Context) (func(string) string, error) {
	if d.su != nil {
		return d.su, nil
	}
	for _, wrap := range suWrappers {
		out, err := d.c.Output(ctx, d.serial, wrap("id"))
		if err != nil {
			return nil, mapErr(err)
		}
		if strings.Contains(string(out), "uid=0") {
			d.su = wrap
			return wrap, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", d.serial, device.ErrNotRooted)
}

func parseProcPartitions(b []byte) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		blocks, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue // header line
		}
		out[f[3]] = blocks * 1024
	}
	return out
}

// sysSize reads the exact size of a block device from
// /sys/class/block/<name>/size (512-byte sectors). /proc/partitions only has
// 1 KiB granularity, so /sys is preferred wherever it resolves.
func (d *Device) sysSize(ctx context.Context, su func(string) string, blockPath string) (int64, bool) {
	name := path.Base(blockPath)
	if !partNameRe.MatchString(name) || name == "." || name == ".." {
		return 0, false
	}
	out, err := d.c.Output(ctx, d.serial, su("cat /sys/class/block/"+name+"/size"))
	if err != nil {
		return 0, false
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || sectors < 0 || sectors > math.MaxInt64/512 {
		return 0, false
	}
	return sectors * 512, true
}

// Partitions lists named partitions (by-name links) or, if none, every block
// device in /proc/partitions. A size missing from /proc/partitions is read
// from /sys; a size that cannot be determined is 0. Requires root.
func (d *Device) Partitions(ctx context.Context) ([]device.Partition, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	su, err := d.rootWrapper(ctx)
	if err != nil {
		return nil, err
	}
	parts, err := d.listPartitions(ctx, su)
	if err != nil {
		return nil, err
	}
	for i := range parts {
		if parts[i].Size == 0 {
			if size, ok := d.sysSize(ctx, su, parts[i].Path); ok {
				parts[i].Size = size
			}
		}
	}
	return parts, nil
}

func (d *Device) listPartitions(ctx context.Context, su func(string) string) ([]device.Partition, error) {
	procOut, err := d.c.Output(ctx, d.serial, su("cat /proc/partitions"))
	if err != nil {
		return nil, mapErr(err)
	}
	sizes := parseProcPartitions(procOut)
	lsOut, err := d.c.Output(ctx, d.serial, su("ls -l /dev/block/by-name/"))
	if err != nil {
		return nil, mapErr(err)
	}
	var parts []device.Partition
	for _, line := range strings.Split(string(lsOut), "\n") {
		before, target, ok := strings.Cut(strings.TrimRight(line, "\r"), " -> ")
		f := strings.Fields(before)
		if !ok || len(f) == 0 {
			continue
		}
		name, target := f[len(f)-1], strings.TrimSpace(target)
		if partNameRe.MatchString(name) && validBlockPath(target) {
			parts = append(parts, device.Partition{Name: name, Path: target, Size: sizes[path.Base(target)]})
		}
	}
	if len(parts) == 0 {
		for name, size := range sizes {
			if p := "/dev/block/" + name; partNameRe.MatchString(name) && validBlockPath(p) {
				parts = append(parts, device.Partition{Name: name, Path: p, Size: size})
			}
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	return parts, nil
}

// ResolvePartition returns the block device a partition name resolves to and
// its exact size in bytes (from /sys when it resolves, else /proc/partitions);
// Size is -1 when the size cannot be determined. Requires root.
func (d *Device) ResolvePartition(ctx context.Context, partition string) (device.Partition, error) {
	if !partNameRe.MatchString(partition) {
		return device.Partition{}, fmt.Errorf("invalid partition name %q", partition)
	}
	if err := d.ready(); err != nil {
		return device.Partition{}, err
	}
	su, err := d.rootWrapper(ctx)
	if err != nil {
		return device.Partition{}, err
	}
	parts, err := d.listPartitions(ctx, su)
	if err != nil {
		return device.Partition{}, err
	}
	for _, p := range parts {
		if p.Name != partition {
			continue
		}
		if size, ok := d.sysSize(ctx, su, p.Path); ok {
			p.Size = size
		} else if p.Size == 0 {
			p.Size = -1
		}
		return p, nil
	}
	return device.Partition{}, fmt.Errorf("no partition named %q on %s", partition, d.serial)
}

// Image streams the raw partition with dd (exec: only, never shell:). A byte
// count different from the partition's exact size is an error, and so is a
// size that cannot be determined: either way the artifact is flagged
// incomplete rather than recorded as a verified image.
func (d *Device) Image(ctx context.Context, partition string, w io.Writer, progress device.ProgressFunc) (int64, error) {
	p, err := d.ResolvePartition(ctx, partition)
	if err != nil {
		return 0, err
	}
	if !validBlockPath(p.Path) {
		return 0, fmt.Errorf("no partition named %q on %s", partition, d.serial)
	}
	rc, err := d.c.Exec(ctx, d.serial, d.su("dd if="+p.Path+" bs=4M 2>/dev/null"))
	if err != nil {
		return 0, mapErr(err)
	}
	defer func() { _ = rc.Close() }()
	n, err := io.Copy(device.WriteCounter(w, p.Size, progress), rc)
	if err != nil {
		return n, err
	}
	if p.Size < 0 {
		return n, fmt.Errorf("%s: size unknown; completeness unverifiable (%d bytes read)", p.Path, n)
	}
	if n != p.Size {
		return n, fmt.Errorf("short read from %s: got %d of %d bytes", p.Path, n, p.Size)
	}
	return n, nil
}
