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

	"howett.net/plist"

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
// "finished"; the returned state is "missing" or "unreadable" when the file
// cannot be used.
func snapshotState(staging, udid string) (string, error) {
	data, err := os.ReadFile(filepath.Join(staging, udid, "Status.plist"))
	if err != nil {
		return "missing", fmt.Errorf("backup snapshot not finished: Status.plist missing: %w", err)
	}
	var st struct {
		SnapshotState string `plist:"SnapshotState"`
	}
	if _, err := plist.Unmarshal(data, &st); err != nil {
		return "unreadable", fmt.Errorf("backup snapshot not finished: Status.plist unreadable: %w", err)
	}
	if st.SnapshotState != "finished" {
		return st.SnapshotState, fmt.Errorf("backup snapshot not finished: %q", st.SnapshotState)
	}
	return st.SnapshotState, nil
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
