package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

var defaultRoots = []string{"/sdcard"}

var infoCommands = []struct{ rel, cmd string }{
	{"device/getprop.txt", "getprop"},
	{"device/packages.txt", "pm list packages -f"},
}

// AcquireLogical captures device properties and the package list, then pulls
// every readable regular file under opts.Roots (default /sdcard).
func (d *Device) AcquireLogical(ctx context.Context, c *evidence.Case, opts device.LogicalOptions, progress device.ProgressFunc) error {
	if err := d.ready(); err != nil {
		return err
	}
	roots := opts.Roots
	if len(roots) == 0 {
		roots = defaultRoots
	}
	return device.RunAcquisition(c, d.serial, "logical", map[string]any{"roots": roots}, func(acq string) error {
		for _, ic := range infoCommands {
			if err := d.captureInfo(ctx, c, acq, ic.rel, ic.cmd); err != nil {
				return err
			}
		}
		s, err := d.sync(ctx)
		if err != nil {
			return err
		}
		w := &walker{
			ctx: ctx, d: d, c: c, acq: acq, s: s, progress: progress,
			names: newLocalPaths(), pulled: map[string]bool{},
		}
		defer func() { _ = w.s.Close() }()
		for _, root := range roots {
			if err := w.walk(root); err != nil {
				return err
			}
		}
		_, err = c.Audit.Append("acquire.stats", d.serial, map[string]any{
			"acquisition_id": acq, "files": w.files, "bytes": w.bytes, "skipped": w.skipped,
		})
		return err
	})
}

type walker struct {
	ctx      context.Context
	d        *Device
	c        *evidence.Case
	acq      string
	s        *adb.Sync
	progress device.ProgressFunc
	names    *localPaths
	pulled   map[string]bool // remote files already captured (overlapping roots)
	files    int
	skipped  int
	bytes    int64
}

func (w *walker) warn(op, p string, cause error) error {
	w.skipped++
	_, err := w.c.Audit.Append("acquire.warning", w.d.serial, map[string]any{
		"acquisition_id": w.acq, "op": op, "path": p, "error": cause.Error(),
	})
	return err
}

// reopen replaces the sync session; adbd closes it after any FAIL.
func (w *walker) reopen() error {
	_ = w.s.Close()
	s, err := w.d.c.Sync(w.ctx, w.d.serial)
	if err != nil {
		return mapErr(err)
	}
	w.s = s
	return nil
}

func isFail(err error) bool {
	var fe *adb.FailError
	return errors.As(err, &fe)
}

// validEntryName rejects LIST names that are not a single path element: an
// empty name or "." would re-list the same directory forever, and "/", ".."
// or NUL could leave the root being walked.
func validEntryName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsRune(n, '/') && !strings.ContainsRune(n, 0)
}

func (w *walker) walk(dir string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	local, err := w.names.dir(dir)
	if err != nil {
		return err
	}
	entries, err := w.s.List(dir)
	if err != nil {
		if !isFail(err) {
			return err
		}
		if err := w.warn("list", dir, err); err != nil {
			return err
		}
		return w.reopen()
	}
	for _, e := range entries {
		if !validEntryName(e.Name) {
			if err := w.warn("skip", dir, fmt.Errorf("invalid entry name %q in listing", e.Name)); err != nil {
				return err
			}
			continue
		}
		p := path.Join(dir, e.Name)
		switch {
		case e.IsDir():
			err = w.walk(p)
		case e.IsRegular():
			err = w.pull(p, local, e)
		default:
			err = w.warn("skip", p, fmt.Errorf("not a regular file (mode %o)", e.Mode))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) pull(p, localDir string, e adb.SyncEntry) error {
	if w.pulled[p] {
		return w.warn("skip", p, errors.New("already acquired in this acquisition (overlapping roots)"))
	}
	w.pulled[p] = true
	rel, err := w.names.file(localDir, e.Name)
	if err != nil {
		return err
	}
	src := evidence.Source{Kind: "file", DeviceID: w.d.serial, RemotePath: p}
	rec, err := w.c.Capture(w.d.serial, w.acq, rel, src, func(out io.Writer) error {
		_, err := w.s.Recv(p, out)
		return err
	})
	switch {
	case err == nil:
		w.files++
		w.bytes += rec.Size
		if w.progress != nil {
			w.progress(w.bytes, -1)
		}
		return nil
	case isFail(err):
		if werr := w.warn("pull", p, err); werr != nil {
			return werr
		}
		return w.reopen()
	default:
		return err
	}
}

// captureInfo streams the output of a device command into an info artifact.
// The command runs via exec: with the shell: fallback; Source.RemotePath
// records the service actually used ("exec:getprop" or "shell:getprop").
func (d *Device) captureInfo(ctx context.Context, c *evidence.Case, acq, rel, cmd string) error {
	rc, svc, err := d.c.Stream(ctx, d.serial, cmd)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = rc.Close() }()
	src := evidence.Source{Kind: "info", DeviceID: d.serial, RemotePath: svc + cmd}
	_, err = c.Capture(d.serial, acq, rel, src, func(w io.Writer) error {
		_, err := io.Copy(w, rc)
		return err
	})
	return err
}
