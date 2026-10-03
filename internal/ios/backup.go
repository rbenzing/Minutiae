package ios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/ios/mb2"
)

// AcquireLogical records lockdown values, then runs a full mobilebackup2
// backup into <case>/staging/<acq>/ and promotes every resulting file into
// artifacts backup/<path>. Files received before a failure are still promoted.
// A backup is complete only if mobilebackup2 reports success and the device's
// Status.plist says SnapshotState == "finished"; otherwise an error is returned
// (and audited as acquire.error) after everything received has been promoted.
func (d *Device) AcquireLogical(ctx context.Context, c *evidence.Case, _ device.LogicalOptions, progress device.ProgressFunc) error {
	return device.RunAcquisition(c, d.udid, "ios.backup", nil, func(acq string) error {
		vals, err := d.b.Values(ctx, d.udid)
		if err != nil {
			return mapErr(err)
		}
		src := evidence.Source{Kind: "info", DeviceID: d.udid, RemotePath: "lockdown:GetValue"}
		if _, err := c.Capture(d.udid, acq, "device/lockdown.json", src, func(w io.Writer) error {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(vals)
		}); err != nil {
			return err
		}

		staging := filepath.Join(c.Dir, "staging", acq)
		if err := os.MkdirAll(staging, 0o750); err != nil {
			return err
		}
		if _, err := c.Audit.Append("acquire.staging", d.udid, map[string]any{
			"acquisition_id": acq, "dir": filepath.ToSlash(filepath.Join("staging", acq)),
		}); err != nil {
			return err
		}

		res, runErr := d.backup(ctx, staging, progress)
		state, snapErr := snapshotState(staging, d.udid)
		if runErr != nil {
			snapErr = nil // the transport failure is the primary error
		}
		promoted, promoteErr := promote(c, d.udid, acq, staging, res.Incomplete)
		_, statsErr := c.Audit.Append("acquire.stats", d.udid, map[string]any{
			"acquisition_id": acq, "files": promoted.files, "incomplete_files": promoted.partial,
			"bytes_received": res.BytesReceived, "snapshot_state": state,
			"local_errors": res.LocalErrors, "local_error_count": res.LocalErrorCount,
			"remote_errors": res.RemoteErrors, "remote_error_count": res.RemoteErrorCount,
		})
		return errors.Join(runErr, snapErr, promoteErr, statsErr)
	})
}

func (d *Device) backup(ctx context.Context, staging string, progress device.ProgressFunc) (mb2.Result, error) {
	rw, err := d.b.OpenBackup(ctx, d.udid)
	if err != nil {
		return mb2.Result{}, mapErr(err)
	}
	defer func() { _ = rw.Close() }()
	// Closing the connection is the only way to interrupt a blocked read, so a
	// device that never answers cannot outlive the context.
	stop := context.AfterFunc(ctx, func() { _ = rw.Close() })
	defer stop()
	client := mb2.New(rw)
	if err := client.Handshake(); err != nil {
		if ctx.Err() != nil {
			return mb2.Result{}, ctx.Err()
		}
		return mb2.Result{}, err
	}
	return client.Backup(ctx, mb2.BackupOptions{
		UDID:      d.udid,
		Dir:       staging,
		FreeSpace: func() (uint64, error) { return freeSpace(staging) },
		Progress: func(n int64) {
			if progress != nil {
				progress(n, -1)
			}
		},
	})
}

// snapshotState reads <staging>/<udid>/Status.plist (XML or binary) and
// returns its SnapshotState. The error is non-nil unless the state is
// "finished". The returned state is "missing" only when the file does not
// exist, and "unreadable" when it exists but cannot be used (not a regular
// file, larger than mb2.MaxPlistFile, unreadable, or not a valid plist). The
// device wrote the file, so it is decoded only through mb2.UnmarshalPlist.
func snapshotState(staging, udid string) (string, error) {
	p := filepath.Join(staging, udid, "Status.plist")
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing", fmt.Errorf("backup snapshot not finished: Status.plist missing: %w", err)
	}
	unreadable := func(err error) (string, error) {
		return "unreadable", fmt.Errorf("backup snapshot not finished: Status.plist unreadable: %w", err)
	}
	switch {
	case err != nil:
		return unreadable(err)
	case !fi.Mode().IsRegular():
		return unreadable(fmt.Errorf("not a regular file (%s)", fi.Mode()))
	case fi.Size() > mb2.MaxPlistFile:
		return unreadable(fmt.Errorf("%d bytes exceeds the %d-byte limit", fi.Size(), mb2.MaxPlistFile))
	}
	data, err := readLimited(p, mb2.MaxPlistFile)
	if err != nil {
		return unreadable(err)
	}
	var st struct {
		SnapshotState string `plist:"SnapshotState"`
	}
	if err := mb2.UnmarshalPlist(data, &st); err != nil {
		return unreadable(err)
	}
	if st.SnapshotState != "finished" {
		return st.SnapshotState, fmt.Errorf("backup snapshot not finished: %q", st.SnapshotState)
	}
	return st.SnapshotState, nil
}

// readLimited reads at most limit bytes of p, failing if the file is longer.
func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("more than %d bytes", limit)
	}
	return b, err
}

// errTransferInterrupted marks a staged file whose transfer failed part-way.
var errTransferInterrupted = errors.New("transfer interrupted")

// promoteStats counts the artifacts promote created; partial ones (transfer
// interrupted, flagged incomplete) are included in files.
type promoteStats struct{ files, partial int }

// promote captures every staged file as an artifact, then removes staging.
// Files named in interrupted (slash-separated, relative to staging) were cut
// off mid-transfer: their bytes are kept as artifacts flagged incomplete,
// which is not a promotion failure. Staging is kept if any file could not be
// promoted, so nothing is lost.
func promote(c *evidence.Case, udid, acq, staging string, interrupted []string) (promoteStats, error) {
	var cut []fs.FileInfo // matched by identity, so case-folding filesystems agree
	for _, rel := range interrupted {
		if fi, err := os.Stat(filepath.Join(staging, filepath.FromSlash(rel))); err == nil {
			cut = append(cut, fi)
		}
	}
	var errs []error
	var st promoteStats
	walkErr := filepath.WalkDir(staging, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src := evidence.Source{Kind: "backup", DeviceID: udid, RemotePath: rel}
		var perr error
		if isInterrupted(p, cut) {
			if perr = capturePartial(c, udid, acq, rel, p, src); perr == nil {
				st.partial++
			}
		} else {
			_, perr = c.Capture(udid, acq, "backup/"+rel, src, func(w io.Writer) error { return copyFrom(w, p) })
		}
		if perr != nil {
			errs = append(errs, perr)
		} else {
			st.files++
		}
		return nil
	})
	if err := errors.Join(append(errs, walkErr)...); err != nil {
		return st, err
	}
	return st, os.RemoveAll(staging)
}

func isInterrupted(p string, cut []fs.FileInfo) bool {
	if len(cut) == 0 {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	for _, c := range cut {
		if os.SameFile(fi, c) {
			return true
		}
	}
	return false
}

// capturePartial stores the bytes of an interrupted file as an artifact
// aborted with errTransferInterrupted, so it is flagged incomplete. The
// error is non-nil only if the staged bytes could not all be stored.
func capturePartial(c *evidence.Case, udid, acq, rel, p string, src evidence.Source) error {
	w, err := c.NewArtifact(udid, acq, "backup/"+rel, src)
	if err != nil {
		return err
	}
	copyErr := copyFrom(w, p)
	_, abortErr := w.Abort(errors.Join(errTransferInterrupted, copyErr))
	return errors.Join(copyErr, abortErr)
}

func copyFrom(w io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}
