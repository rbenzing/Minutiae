package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// importFile is one validated source file, held open for the whole import so
// the audited size and the copied bytes come from the same handle.
type importFile struct {
	f     *os.File
	path  string // absolute
	size  int64
	mtime string // RFC 3339 UTC
	rel   string // artifact path below the acquisition directory
}

// Import copies external image files into the case as one acquisition with
// segments 1..N (Kind "import", OriginalPath = absolute path, Segment = i+1,
// RemoteSize = size, RemoteMTime = file mtime RFC 3339 UTC), bracketed by
// acquire.start/acquire.end audit entries. All inputs are validated before
// anything is written. progress (optional) receives the bytes copied so far
// and the total. A cancelled ctx or a read failure keeps the partial artifact
// flagged incomplete and returns the records written so far with the error.
func Import(ctx context.Context, c *evidence.Case, deviceID string, paths []string, progress func(done, total int64)) ([]evidence.ManifestRecord, error) {
	if strings.TrimSpace(deviceID) == "" {
		return nil, errors.New("import needs a device id")
	}
	if len(paths) == 0 {
		return nil, errors.New("no image files to import")
	}
	files, total, err := openImportFiles(c, paths)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, f := range files {
			_ = f.f.Close()
		}
	}()

	details := make([]map[string]any, len(files))
	for i, f := range files {
		details[i] = map[string]any{"path": f.path, "size": f.size, "mtime": f.mtime}
	}
	var recs []evidence.ManifestRecord
	var done int64
	err = device.RunAcquisition(c, deviceID, "import", map[string]any{"method": "import", "files": details}, func(acqID string) error {
		for i, f := range files {
			src := evidence.Source{
				Kind: "import", DeviceID: deviceID, OriginalPath: f.path, Segment: i + 1,
				RemoteSize: f.size, RemoteMTime: f.mtime,
			}
			rec, err := c.Capture(deviceID, acqID, f.rel, src, func(w io.Writer) error {
				n, err := io.Copy(w, &ctxReader{ctx: ctx, r: f.f, onRead: func(n int) {
					done += int64(n)
					if progress != nil {
						progress(done, total)
					}
				}})
				if err == nil && n != f.size {
					err = fmt.Errorf("%s changed while it was being read: copied %d bytes, expected %d", f.path, n, f.size)
				}
				return err
			})
			if rec.ID != "" {
				recs = append(recs, rec)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return recs, err
}

// openImportFiles validates and opens every input (read-only).
func openImportFiles(c *evidence.Case, paths []string) (files []importFile, total int64, err error) {
	defer func() {
		if err != nil {
			for _, f := range files {
				_ = f.f.Close()
			}
			files, total = nil, 0
		}
	}()
	caseDirs := dirForms(c.Dir)
	used := map[string]bool{}
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return files, 0, err
		}
		if insideAny(caseDirs, dirForms(abs)) {
			return files, 0, fmt.Errorf("cannot import from inside the case: %s", abs)
		}
		f, err := os.Open(abs) // read-only by construction
		if err != nil {
			return files, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return files, 0, err
		}
		if !st.Mode().IsRegular() {
			_ = f.Close()
			return files, 0, fmt.Errorf("%s is not a regular file", abs)
		}
		rel := "image/" + filepath.Base(abs)
		if used[rel] {
			rel = fmt.Sprintf("image/%d-%s", i+1, filepath.Base(abs))
		}
		used[rel] = true
		total += st.Size()
		files = append(files, importFile{f: f, path: abs, size: st.Size(), mtime: st.ModTime().UTC().Format(time.RFC3339), rel: rel})
	}
	return files, total, nil
}

// dirForms returns p as an absolute path and, when it resolves differently,
// with symlinks (and short-name aliases) resolved, so a link into the case is
// still caught.
func dirForms(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	forms := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		forms = append(forms, resolved)
	}
	return forms
}

func insideAny(dirs, targets []string) bool {
	for _, d := range dirs {
		for _, t := range targets {
			if insideDir(d, t) {
				return true
			}
		}
	}
	return false
}

// insideDir reports whether p is dir itself or lies below it.
func insideDir(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// ctxReader fails reads once ctx is cancelled and reports bytes read.
type ctxReader struct {
	ctx    context.Context
	r      io.Reader
	onRead func(n int)
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if n > 0 && r.onRead != nil {
		r.onRead(n)
	}
	return n, err
}
