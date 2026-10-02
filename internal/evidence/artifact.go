package evidence

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ErrArtifactExists is returned instead of overwriting an existing artifact.
var ErrArtifactExists = errors.New("artifact already exists")

var errArtifactClosed = errors.New("artifact already closed")

// NewAcquisitionID returns a sortable, filesystem-safe id for one acquisition:
// the UTC timestamp plus 4 random bytes, because clock granularity (notably on
// Windows) makes timestamp-only ids collide.
func NewAcquisitionID(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return t.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:])
}

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeComponent makes one path element safe on Windows, macOS and Linux.
// The original name is preserved in the manifest Source, never lost.
func sanitizeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if trimmed := strings.TrimRight(out, " ."); trimmed != out {
		out = trimmed + "_"
	}
	if out == "" {
		out = "_"
	}
	if windowsReserved[strings.ToUpper(strings.SplitN(out, ".", 2)[0])] {
		out = "_" + out
	}
	return out
}

// SanitizeRelPath validates a slash-separated relative path and sanitizes each
// element. It rejects absolute paths and any ".." element.
func SanitizeRelPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || hasDriveLetter(p) {
		return "", fmt.Errorf("artifact path %q must be relative", p)
	}
	var out []string
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("artifact path %q escapes the artifact directory", p)
		}
		out = append(out, sanitizeComponent(part))
	}
	if len(out) == 0 {
		return "", fmt.Errorf("artifact path %q is empty", p)
	}
	return strings.Join(out, "/"), nil
}

// hasDriveLetter reports a Windows drive prefix such as "C:", "C:/x" or `C:\x`.
// A colon elsewhere ("a:b") is just a character to sanitize.
func hasDriveLetter(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20
	if c < 'a' || c > 'z' {
		return false
	}
	return len(p) == 2 || p[2] == '/' || p[2] == '\\'
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b[:])
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ArtifactWriter streams bytes into one exclusive, hashed artifact file.
type ArtifactWriter struct {
	c    *Case
	f    *os.File
	h    *MultiHasher
	rec  ManifestRecord
	done bool
}

// NewArtifact creates artifacts/<device>/<acq>/<relPath> exclusively.
func (c *Case) NewArtifact(deviceID, acqID, relPath string, src Source) (*ArtifactWriter, error) {
	rel, err := SanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	if deviceID == "" || acqID == "" {
		return nil, errors.New("artifact needs a device id and acquisition id")
	}
	caseRel := path.Join(artifactsDir, sanitizeComponent(deviceID), sanitizeComponent(acqID), rel)
	full := filepath.Join(c.Dir, filepath.FromSlash(caseRel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrArtifactExists, caseRel)
	}
	if err != nil {
		return nil, err
	}
	return &ArtifactWriter{
		c: c, f: f, h: NewMultiHasher(),
		rec: ManifestRecord{ID: newID(), Path: caseRel, Source: src, Started: nowUTC()},
	}, nil
}

// Path is the artifact's slash-separated path relative to the case dir.
func (w *ArtifactWriter) Path() string { return w.rec.Path }

func (w *ArtifactWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, errArtifactClosed
	}
	n, err := w.f.Write(p)
	_, _ = w.h.Write(p[:n])
	return n, err
}

// Close finalizes a complete artifact.
func (w *ArtifactWriter) Close() (ManifestRecord, error) { return w.finish(nil) }

// Abort finalizes a partial artifact: the file is kept and flagged incomplete.
func (w *ArtifactWriter) Abort(cause error) (ManifestRecord, error) {
	if cause == nil {
		cause = errors.New("aborted")
	}
	return w.finish(cause)
}

func (w *ArtifactWriter) finish(cause error) (ManifestRecord, error) {
	if w.done {
		return w.rec, errArtifactClosed
	}
	w.done = true
	fileErr := errors.Join(w.f.Sync(), w.f.Close())
	d := w.h.Sum()
	w.rec.Size, w.rec.SHA256, w.rec.MD5 = d.Size, d.SHA256, d.MD5
	w.rec.Finished = nowUTC()
	if failure := errors.Join(cause, fileErr); failure != nil {
		w.rec.Incomplete = true
		w.rec.Error = failure.Error()
	}
	if err := w.c.recordArtifact(w.rec); err != nil {
		return w.rec, errors.Join(fileErr, err)
	}
	return w.rec, fileErr
}

func (c *Case) recordArtifact(r ManifestRecord) error {
	c.manifestMu.Lock()
	defer c.manifestMu.Unlock()
	if err := appendManifest(filepath.Join(c.Dir, manifestFile), r); err != nil {
		return err
	}
	if err := c.store.InsertArtifact(r); err != nil {
		return err
	}
	_, err := c.Audit.Append("artifact.create", r.Source.DeviceID, map[string]any{
		"id": r.ID, "path": r.Path, "size": r.Size, "sha256": r.SHA256, "md5": r.MD5,
		"source": r.Source, "incomplete": r.Incomplete, "error": r.Error,
	})
	return err
}

// Capture creates an artifact, lets fill write it, then closes it — or aborts
// it (keeping the partial bytes) if fill fails.
func (c *Case) Capture(deviceID, acqID, relPath string, src Source, fill func(io.Writer) error) (ManifestRecord, error) {
	w, err := c.NewArtifact(deviceID, acqID, relPath, src)
	if err != nil {
		return ManifestRecord{}, err
	}
	if err := fill(w); err != nil {
		rec, aerr := w.Abort(err)
		return rec, errors.Join(err, aerr)
	}
	return w.Close()
}
