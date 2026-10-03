package mb2

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type handler struct {
	c   *Client
	o   BackupOptions
	res Result
}

func (h *handler) run() error {
	if err := h.c.processMessage(map[string]any{
		"MessageName": "Backup", "TargetIdentifier": h.o.UDID, "SourceIdentifier": h.o.UDID,
		"Options": map[string]any{"ForceFullBackup": true},
	}); err != nil {
		return err
	}
	for {
		msg, err := h.c.codec.Recv()
		if err != nil {
			return fmt.Errorf("mobilebackup2: %w", err)
		}
		switch Name(msg) {
		case "DLMessageDownloadFiles":
			err = h.sendFiles(msg)
		case "DLMessageUploadFiles":
			err = h.receiveFiles()
		case "DLMessageGetFreeDiskSpace":
			err = h.freeSpace()
		case "DLContentsOfDirectory":
			err = h.listDir(msg)
		case "DLMessageCreateDirectory":
			err = h.mkdir(msg)
		case "DLMessageMoveFiles", "DLMessageMoveItems":
			err = h.move(msg)
		case "DLMessageRemoveFiles", "DLMessageRemoveItems":
			err = h.remove(msg)
		case "DLMessageCopyItem":
			err = h.copyItem(msg)
		case "DLMessageDisconnect":
			return nil
		case "DLMessageProcessMessage":
			_, perr := checkProcessMessage(msg)
			_ = h.c.codec.Send([]any{"DLMessageDisconnect", EmptyParameter})
			return perr
		default:
			return fmt.Errorf("mobilebackup2: unsupported message %q", Name(msg))
		}
		if err != nil {
			return fmt.Errorf("mobilebackup2 %s: %w", Name(msg), err)
		}
	}
}

// local maps a device-relative path into o.Dir, refusing anything that escapes it.
func (h *handler) local(p string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(p))
	if p == "" || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe device path %q", p)
	}
	return filepath.Join(h.o.Dir, clean), nil
}

// deviceErrno mirrors libimobiledevice's errno_to_device_error.
func deviceErrno(err error) int64 {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return -6
	case errors.Is(err, fs.ErrExist):
		return -7
	default:
		return -1
	}
}

func (h *handler) status(code int64, desc string, payload any) error {
	if desc == "" {
		desc = EmptyParameter
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return h.c.codec.Send([]any{"DLMessageStatusResponse", code, desc, payload})
}

func (h *handler) statusFor(err error) error {
	if err == nil {
		return h.status(0, "", nil)
	}
	return h.status(deviceErrno(err), err.Error(), nil)
}

func (h *handler) sendFiles(msg []any) error {
	paths, _ := arg(msg, 1).([]any)
	fileErrs := map[string]any{}
	for _, pv := range paths {
		p, _ := pv.(string)
		if err := WriteName(h.c.rw, p); err != nil {
			return err
		}
		localErr, ioErr := h.sendFile(p)
		if ioErr != nil {
			return ioErr
		}
		if localErr != nil {
			fileErrs[p] = map[string]any{"DLFileErrorString": localErr.Error(), "DLFileErrorCode": deviceErrno(localErr)}
			if err := WriteBlock(h.c.rw, CodeErrorLocal, []byte(localErr.Error())); err != nil {
				return err
			}
		}
	}
	if err := WriteU32(h.c.rw, 0); err != nil {
		return err
	}
	if len(fileErrs) > 0 {
		return h.status(-13, "Multi status", fileErrs)
	}
	return h.status(0, "", nil)
}

func (h *handler) sendFile(p string) (localErr, ioErr error) {
	src, err := h.local(p)
	if err != nil {
		return err, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return err, nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, chunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := WriteBlock(h.c.rw, CodeFileData, buf[:n]); err != nil {
				return nil, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr, nil
		}
	}
	return nil, WriteBlock(h.c.rw, CodeSuccess, nil)
}

func (h *handler) receiveFiles() error {
	r := h.c.rw
	for {
		dname, err := ReadName(r)
		if err != nil {
			return err
		}
		if dname == "" {
			break
		}
		fname, err := ReadName(r)
		if err != nil {
			return err
		}
		w := io.Discard
		var f *os.File
		dst, lerr := h.local(fname)
		if lerr == nil {
			lerr = os.MkdirAll(filepath.Dir(dst), 0o750)
		}
		if lerr == nil {
			if f, lerr = os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); lerr == nil {
				w = f
			}
		}
		if lerr != nil {
			h.res.LocalErrors = append(h.res.LocalErrors, fmt.Sprintf("%s: %v", fname, lerr))
		}
		err = h.receiveBlocks(r, w, fname)
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			return err
		}
	}
	return h.status(0, "", nil)
}

func (h *handler) receiveBlocks(r io.Reader, w io.Writer, fname string) error {
	last := byte(0xff)
	for {
		n, err := ReadU32(r)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		code, err := ReadByte(r)
		if err != nil {
			return err
		}
		payload := int64(n) - 1
		switch code {
		case CodeFileData:
			copied, err := io.CopyN(w, r, payload)
			h.res.BytesReceived += copied
			if h.o.Progress != nil {
				h.o.Progress(h.res.BytesReceived)
			}
			if err != nil {
				return err
			}
			last = code
		case CodeSuccess:
			_, err := io.CopyN(io.Discard, r, payload)
			return err
		case CodeErrorRemote, CodeErrorLocal:
			msg := make([]byte, payload)
			if _, err := io.ReadFull(r, msg); err != nil {
				return err
			}
			if last != CodeFileData { // after data, 0x0b is just the end marker
				h.res.RemoteErrors = append(h.res.RemoteErrors, fmt.Sprintf("%s: %s", fname, msg))
			}
			return nil
		default:
			return fmt.Errorf("unknown block code 0x%02x for %s", code, fname)
		}
	}
}

func (h *handler) freeSpace() error {
	if h.o.FreeSpace == nil {
		return h.status(-1, "free space unknown", uint64(0))
	}
	n, err := h.o.FreeSpace()
	if err != nil {
		return h.status(-1, err.Error(), uint64(0))
	}
	return h.status(0, "", n)
}

func (h *handler) listDir(msg []any) error {
	out := map[string]any{}
	p, _ := arg(msg, 1).(string)
	if dir, err := h.local(p); err == nil {
		entries, _ := os.ReadDir(dir) // a missing directory lists as empty
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			t := "DLFileTypeUnknown"
			switch {
			case info.IsDir():
				t = "DLFileTypeDirectory"
			case info.Mode().IsRegular():
				t = "DLFileTypeRegular"
			}
			out[e.Name()] = map[string]any{"DLFileType": t, "DLFileSize": uint64(info.Size()), "DLFileModificationDate": info.ModTime()}
		}
	}
	return h.status(0, "", out)
}

func (h *handler) mkdir(msg []any) error {
	p, _ := arg(msg, 1).(string)
	dir, err := h.local(p)
	if err == nil {
		err = os.MkdirAll(dir, 0o750)
	}
	return h.statusFor(err)
}

func (h *handler) move(msg []any) error {
	items, _ := arg(msg, 1).(map[string]any)
	var errs []error
	for from, tov := range items {
		to, _ := tov.(string)
		src, e1 := h.local(from)
		dst, e2 := h.local(to)
		if err := errors.Join(e1, e2); err != nil {
			errs = append(errs, err)
			continue
		}
		_ = os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, err)
		}
	}
	return h.statusFor(errors.Join(errs...))
}

func (h *handler) remove(msg []any) error {
	items, _ := arg(msg, 1).([]any)
	var errs []error
	for _, v := range items {
		p, _ := v.(string)
		target, err := h.local(p)
		if err == nil {
			err = os.RemoveAll(target)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return h.statusFor(errors.Join(errs...))
}

func (h *handler) copyItem(msg []any) error {
	from, _ := arg(msg, 1).(string)
	to, _ := arg(msg, 2).(string)
	src, e1 := h.local(from)
	dst, e2 := h.local(to)
	err := errors.Join(e1, e2)
	if err == nil {
		err = copyPath(src, dst)
	}
	return h.statusFor(err)
}

func copyPath(src, dst string) error {
	var files []string // relative paths; copied after the walk completes
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return err
	}
	for _, rel := range files {
		if err := copyFile(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}
