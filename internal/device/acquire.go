package device

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// RunAcquisition brackets fn with acquire.start and acquire.end/acquire.error
// audit entries sharing one acquisition id.
func RunAcquisition(c *evidence.Case, deviceID, acqType string, details map[string]any, fn func(acqID string) error) error {
	acqID := evidence.NewAcquisitionID(time.Now())
	d := maps.Clone(details)
	if d == nil {
		d = map[string]any{}
	}
	d["acquisition_id"], d["type"] = acqID, acqType
	if _, err := c.Audit.Append("acquire.start", deviceID, d); err != nil {
		return err
	}
	if err := fn(acqID); err != nil {
		_, aerr := c.Audit.Append("acquire.error", deviceID, map[string]any{"acquisition_id": acqID, "error": err.Error()})
		return errors.Join(err, aerr)
	}
	_, err := c.Audit.Append("acquire.end", deviceID, map[string]any{"acquisition_id": acqID})
	return err
}

// PullToCase copies one remote file into artifact files/<remotePath>.
func PullToCase(ctx context.Context, c *evidence.Case, t FileTransferer, deviceID, acqID, remotePath string) (evidence.ManifestRecord, error) {
	src := evidence.Source{Kind: "file", DeviceID: deviceID, RemotePath: remotePath}
	return c.Capture(deviceID, acqID, "files/"+strings.TrimPrefix(remotePath, "/"), src, func(w io.Writer) error {
		_, err := t.Pull(ctx, remotePath, w)
		return err
	})
}

// ImageToCase images one partition into artifact partitions/<name>.img.
func ImageToCase(ctx context.Context, c *evidence.Case, im Imager, deviceID, acqID, partition string, progress ProgressFunc) (evidence.ManifestRecord, error) {
	src := evidence.Source{Kind: "partition", DeviceID: deviceID, Partition: partition}
	return c.Capture(deviceID, acqID, "partitions/"+partition+".img", src, func(w io.Writer) error {
		_, err := im.Image(ctx, partition, w, progress)
		return err
	})
}

// PushAudited writes a local file to the device. It refuses unless allow is
// true, and records a device.modify audit entry before any byte is sent.
func PushAudited(ctx context.Context, c *evidence.Case, t FileTransferer, deviceID, localPath, remotePath string, allow bool) error {
	if !allow {
		return ErrDeviceWriteNotAllowed
	}
	d, err := evidence.HashFile(localPath)
	if err != nil {
		return err
	}
	details := map[string]any{
		"operation": "push", "local_path": localPath, "remote_path": remotePath,
		"size": d.Size, "sha256": d.SHA256,
	}
	if _, err := c.Audit.Append("device.modify", deviceID, details); err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := t.Push(ctx, f, remotePath, 0o644); err != nil {
		_, aerr := c.Audit.Append("device.modify.error", deviceID, map[string]any{"remote_path": remotePath, "error": err.Error()})
		return errors.Join(err, aerr)
	}
	_, err = c.Audit.Append("device.modify.done", deviceID, map[string]any{"remote_path": remotePath})
	return err
}
