package android

import (
	"context"
	"fmt"
	"io"
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

// suWrappers are tried in order: Magisk/SuperSU style, then AOSP su.
var suWrappers = []func(string) string{
	func(cmd string) string { return "su -c '" + cmd + "'" },
	func(cmd string) string { return "su 0 sh -c '" + cmd + "'" },
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

// Partitions lists named partitions (by-name links) or, if none, every block
// device in /proc/partitions. Requires root.
func (d *Device) Partitions(ctx context.Context) ([]device.Partition, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	su, err := d.rootWrapper(ctx)
	if err != nil {
		return nil, err
	}
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
		if partNameRe.MatchString(name) && blockPathRe.MatchString(target) {
			parts = append(parts, device.Partition{Name: name, Path: target, Size: sizes[path.Base(target)]})
		}
	}
	if len(parts) == 0 {
		for name, size := range sizes {
			if partNameRe.MatchString(name) {
				parts = append(parts, device.Partition{Name: name, Path: "/dev/block/" + name, Size: size})
			}
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	return parts, nil
}

// Image streams the raw partition with dd. A byte count different from the
// size reported by /proc/partitions is an error, so the artifact is flagged
// incomplete rather than silently truncated.
func (d *Device) Image(ctx context.Context, partition string, w io.Writer, progress device.ProgressFunc) (int64, error) {
	if !partNameRe.MatchString(partition) {
		return 0, fmt.Errorf("invalid partition name %q", partition)
	}
	parts, err := d.Partitions(ctx)
	if err != nil {
		return 0, err
	}
	var p *device.Partition
	for i := range parts {
		if parts[i].Name == partition {
			p = &parts[i]
		}
	}
	if p == nil || !blockPathRe.MatchString(p.Path) {
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
	if p.Size > 0 && n != p.Size {
		return n, fmt.Errorf("short read from %s: got %d of %d bytes", p.Path, n, p.Size)
	}
	return n, nil
}
