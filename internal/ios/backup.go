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
		promoted, promoteErr := promote(c, d.udid, acq, staging)
		_, statsErr := c.Audit.Append("acquire.stats", d.udid, map[string]any{
			"acquisition_id": acq, "files": promoted, "bytes_received": res.BytesReceived,
			"local_errors": res.LocalErrors, "remote_errors": res.RemoteErrors,
			"snapshot_state": state,
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
	client := mb2.New(rw)
	if err := client.Handshake(); err != nil {
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

// promote captures every staged file as an artifact, then removes staging.
// Staging is kept if any file could not be promoted, so nothing is lost.
func promote(c *evidence.Case, udid, acq, staging string) (int, error) {
	var errs []error
	count := 0
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
		_, cerr := c.Capture(udid, acq, "backup/"+rel, src, func(w io.Writer) error {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(w, f)
			return err
		})
		if cerr != nil {
			errs = append(errs, cerr)
		} else {
			count++
		}
		return nil
	})
	if err := errors.Join(append(errs, walkErr)...); err != nil {
		return count, err
	}
	return count, os.RemoveAll(staging)
}
