package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// maxNameRetries bounds the alternative local names tried for one remote file
// whose local name collides on disk.
const maxNameRetries = 100

var defaultRoots = []string{"/sdcard"}

// Defaults bound what a hostile device can make a logical acquisition do. They
// are far above any real phone; a hit is a typed warning, never a silent cut.
const (
	DefaultMaxEntriesPerDir = 1_000_000
	DefaultMaxTotalEntries  = 10_000_000
	DefaultMaxDepth         = 128
	DefaultMaxPathBytes     = 512 << 20 // bytes of path text seen: the walker remembers every pulled path (W14)
)

// WalkLimits are the caps of a logical acquisition; zero fields take the defaults.
type WalkLimits struct{ MaxEntriesPerDir, MaxTotalEntries, MaxDepth, MaxPathBytes int }

func (l WalkLimits) withDefaults() WalkLimits {
	if l.MaxEntriesPerDir <= 0 {
		l.MaxEntriesPerDir = DefaultMaxEntriesPerDir
	}
	if l.MaxTotalEntries <= 0 {
		l.MaxTotalEntries = DefaultMaxTotalEntries
	}
	if l.MaxPathBytes <= 0 {
		l.MaxPathBytes = DefaultMaxPathBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	return l
}

// ErrAcquisitionLimited is wrapped by the error of an acquisition that hit a
// walk limit. Everything pulled before the limit is kept.
var ErrAcquisitionLimited = errors.New("acquisition incomplete: a safety limit was reached")

const (
	reasonEntriesPerDir = "entries-per-dir"
	reasonEntriesTotal  = "entries-total"
	reasonDepth         = "depth"
	reasonPathBytes     = "path-bytes"
)

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
			names: evidence.NewLocalPaths("files"), pulled: map[string]bool{},
			lim: d.limits.withDefaults(), hits: map[string]int{},
		}
		defer func() { _ = w.s.Close() }()
		v1 := s.Version() == 1
		if v1 {
			// Sizes and times of a v1 listing are 32-bit: say so as evidence, once.
			if _, err := c.Audit.Append("acquire.warning", d.serial, map[string]any{
				"acquisition_id": acq, "op": "sync", "reason": "sync-v1",
				"error": syncV1Limitation + " (" + s.FallbackReason() + ")",
			}); err != nil {
				return err
			}
		}
		for _, root := range roots {
			if err := w.walk(root, 0); err != nil {
				// Files pulled before the failure stay in the case: say that the
				// acquisition is incomplete before acquire.error is written.
				stats := w.stats(v1, s)
				stats["incomplete"], stats["error"] = true, err.Error()
				_, serr := c.Audit.Append("acquire.stats", d.serial, stats)
				return errors.Join(err, serr)
			}
		}
		if _, err = c.Audit.Append("acquire.stats", d.serial, w.stats(v1, s)); err != nil {
			return err
		}
		if len(w.hits) > 0 {
			return fmt.Errorf("%w: %s", ErrAcquisitionLimited, describeHits(w.hits))
		}
		return nil
	})
}

// syncV1Limitation is the disclosure recorded whenever a v1 sync session is used.
const syncV1Limitation = "sync v1 reports 32-bit sizes and times: file sizes of 4 GiB or more are truncated, so no remote size is recorded; a failed stat is indistinguishable from a missing path; an unreadable directory is only detected when it lists no entry at all, so an empty listing is not evidence of an empty directory"

func describeHits(hits map[string]int) string {
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, hits[k]))
	}
	return strings.Join(parts, ", ")
}

type walker struct {
	ctx      context.Context
	d        *Device
	c        *evidence.Case
	acq      string
	s        *adb.Sync
	progress device.ProgressFunc
	names    *evidence.LocalPaths
	pulled   map[string]bool // remote files already captured (overlapping roots)
	files    int
	skipped  int
	bytes    int64

	lim       WalkLimits
	total     int  // entries listed so far
	pathBytes int  // bytes of path text seen
	halted    bool // a total or path-bytes cap was hit: nothing more is listed or pulled
	hits      map[string]int
}

func (w *walker) warn(op, p string, cause error) error { return w.warnReason(op, p, "", cause) }

func (w *walker) warnReason(op, p, reason string, cause error) error {
	w.skipped++
	details := map[string]any{"acquisition_id": w.acq, "op": op, "path": p, "error": cause.Error()}
	if reason != "" {
		details["reason"] = reason
	}
	_, err := w.c.Audit.Append("acquire.warning", w.d.serial, details)
	return err
}

// limit records a hit of a walk limit as a typed, audited warning.
func (w *walker) limit(reason, p string, cause error) error {
	w.hits[reason]++
	return w.warnReason("limit", p, reason, cause)
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

func (w *walker) walk(dir string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.halted {
		return nil
	}
	if depth > w.lim.MaxDepth {
		return w.limit(reasonDepth, dir, fmt.Errorf("%d levels below the acquisition root; the limit is %d", depth, w.lim.MaxDepth))
	}
	local, err := w.names.Dir(dir)
	if err != nil {
		return err
	}
	res, err := w.s.ListN(dir, w.lim.MaxEntriesPerDir)
	switch {
	case err == nil:
	case errors.Is(err, adb.ErrListLimit):
		// The first entries are processed; the session has frames pending and is replaced.
		if err := w.limit(reasonEntriesPerDir, dir, err); err != nil {
			return err
		}
		if err := w.reopen(); err != nil {
			return err
		}
	case isFail(err):
		if err := w.warn("list", dir, err); err != nil {
			return err
		}
		return w.reopen()
	default:
		return err
	}
	if err := w.listProblems(dir, res); err != nil {
		return err
	}
	w.total += len(res.Entries)
	if w.total > w.lim.MaxTotalEntries {
		w.halted = true
		return w.limit(reasonEntriesTotal, dir, fmt.Errorf("%d entries listed so far; the limit is %d", w.total, w.lim.MaxTotalEntries))
	}
	for _, e := range res.Entries {
		if w.halted {
			return nil
		}
		w.pathBytes += len(dir) + 1 + len(e.Name)
		if w.pathBytes > w.lim.MaxPathBytes {
			w.halted = true
			return w.limit(reasonPathBytes, dir, fmt.Errorf("%d bytes of paths seen; the limit is %d", w.pathBytes, w.lim.MaxPathBytes))
		}
		if !validEntryName(e.Name) {
			if err := w.warn("skip", dir, fmt.Errorf("invalid entry name %q in listing", e.Name)); err != nil {
				return err
			}
			continue
		}
		p := path.Join(dir, e.Name)
		switch {
		case e.IsDir():
			err = w.walk(p, depth+1)
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
	src := remoteSource(w.d.serial, p, e)
	var rec evidence.ManifestRecord
	var err error
	for attempt := 0; ; attempt++ {
		var rel string
		if rel, err = w.names.File(localDir, e.Name); err != nil {
			return err
		}
		rec, err = w.c.Capture(w.d.serial, w.acq, rel, src, func(out io.Writer) error {
			n, err := w.s.Recv(p, out)
			if err == nil && e.V2 && n != e.Size {
				return fmt.Errorf("%w: received %d bytes, listed %d", errSizeMismatch, n, e.Size)
			}
			return err
		})
		if !errors.Is(err, evidence.ErrArtifactExists) {
			break
		}
		// The examiner's filesystem aliases the name in a way LocalPaths does
		// not model (NTFS 8.3 short names, APFS NFC/NFD). The exclusive create
		// failed before anything was requested from the device, so the sync
		// session is untouched; the colliding name stays reserved and the
		// next candidate is tried.
		if attempt >= maxNameRetries {
			return w.warn("skip", p, fmt.Errorf("no free local name after %d attempts: %w", attempt+1, err))
		}
	}
	switch {
	case err == nil:
		w.files++
		w.bytes += rec.Size
		if w.progress != nil {
			w.progress(w.bytes, -1)
		}
		return nil
	case errors.Is(err, errSizeMismatch):
		// Recv ended cleanly, so the session is intact; Capture kept the artifact
		// and flagged it incomplete.
		w.bytes += rec.Size
		return w.warnReason("pull", p, "size-mismatch", err)
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

// remoteSource is the manifest source of a listed file. The size is recorded
// only from a v2 listing (a v1 size may be truncated to 32 bits, so it is
// unknown, not recorded), whatever session is current when the file is pulled.
func remoteSource(serial, p string, e adb.SyncEntry) evidence.Source {
	src := evidence.Source{Kind: "file", DeviceID: serial, RemotePath: p, RemoteMode: e.Mode}
	if !e.MTimeUnknown {
		src.RemoteMTime = e.MTime.UTC().Format(time.RFC3339)
	}
	if e.V2 {
		src.RemoteSize = e.Size
	}
	return src
}

// errSizeMismatch means a v2 listing declared another size than Recv delivered.
var errSizeMismatch = errors.New("received bytes differ from the size the device listed")

// maxEntryWarnings bounds the audited per-entry warnings of one directory; the
// rest are counted in one summary entry.
const maxEntryWarnings = 1000

// listProblems audits what a listing could not read: an unreadable directory
// and an empty answer are never an empty directory (unknown is not absent).
func (w *walker) listProblems(dir string, res adb.ListResult) error {
	if res.DirErrno != 0 {
		if err := w.warnReason("list", dir, "list-errno", &adb.ErrnoError{Errno: res.DirErrno}); err != nil {
			return err
		}
	} else if res.Raw == 0 {
		if err := w.warnReason("list", dir, "list-unknown", adb.ErrListUnknown); err != nil {
			return err
		}
	}
	for i, ee := range res.EntryErrors {
		if i == maxEntryWarnings {
			return w.warnReason("stat", dir, "entry-errno", fmt.Errorf("%d further entries could not be read; not listed individually", len(res.EntryErrors)-maxEntryWarnings))
		}
		if err := w.warnReason("stat", path.Join(dir, ee.Name), "entry-errno", &adb.ErrnoError{Errno: ee.Errno}); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) stats(v1 bool, s *adb.Sync) map[string]any {
	stats := map[string]any{
		"acquisition_id": w.acq, "files": w.files, "bytes": w.bytes, "skipped": w.skipped,
		"incomplete": len(w.hits) > 0, "limit_hits": w.hits, "sync_v1": v1,
	}
	if v1 {
		stats["sync_fallback"] = s.FallbackReason()
	}
	return stats
}
