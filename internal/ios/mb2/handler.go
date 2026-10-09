package mb2

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type handler struct {
	c   *Client
	o   BackupOptions
	res Result

	detailed int // RemoteEnded entries that kept their message
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
		case "DLMessagePurgeDiskSpace":
			err = h.status(-1, "Operation not supported", nil)
		case "DLMessageDisconnect":
			return errors.New("mobilebackup2: device disconnected before completion")
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
	if p == "" || clean == "." || !filepath.IsLocal(clean) {
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
	case errors.Is(err, syscall.ENOTDIR):
		return -8
	case errors.Is(err, syscall.EISDIR):
		return -9
	case errors.Is(err, syscall.ELOOP):
		return -10
	case errors.Is(err, syscall.EIO):
		return -11
	case errors.Is(err, syscall.ENOSPC):
		return -15
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
		if p == "" { // a zero-length name would terminate the list early
			continue
		}
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
		rel := ""
		if lerr == nil {
			rel, lerr = filepath.Rel(h.o.Dir, dst)
		}
		if lerr == nil {
			lerr = os.MkdirAll(filepath.Dir(dst), 0o750)
		}
		if lerr == nil {
			if f, lerr = os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); lerr == nil {
				w = f
			}
		}
		if lerr != nil {
			h.res.LocalErrorCount++
			h.res.LocalErrors = appendCapped(h.res.LocalErrors, fmt.Sprintf("%s: %v", fname, lerr))
		}
		be, err := h.receiveBlocks(r, w, fname)
		if f != nil {
			err = errors.Join(err, f.Close())
			if err != nil { // the file holds only what arrived before the failure
				h.res.Incomplete = append(h.res.Incomplete, filepath.ToSlash(rel))
			}
		}
		if err != nil {
			return err
		}
		if lerr == nil {
			h.noteEnd(filepath.ToSlash(rel), be)
		}
		if be.ended {
			break
		}
	}
	return h.status(0, "", nil)
}

// blockEnd is how one file's blocks ended.
type blockEnd struct {
	ended     bool   // a zero length where a block was expected ends the whole upload
	afterData bool   // an error block followed file data
	code      byte   // that block's code
	msg       string // that block's message, bounded
	fileBytes int64  // file data received in this transfer
}

// receiveBlocks reads one file's blocks. be.ended reports a zero length where
// a block was expected, which (as in libimobiledevice) ends the whole upload.
func (h *handler) receiveBlocks(r io.Reader, w io.Writer, fname string) (blockEnd, error) {
	var be blockEnd
	last := byte(0xff)
	for {
		n, err := ReadU32(r)
		if err != nil {
			return be, err
		}
		if n == 0 {
			be.ended = true
			if last == CodeFileData { // data with no terminal block: the transfer was interrupted
				be.afterData, be.code, be.msg = true, CodeNoTerminator, "zero length after file data, no success block"
			}
			return be, nil
		}
		code, err := ReadByte(r)
		if err != nil {
			return be, err
		}
		payload := int64(n) - 1
		switch code {
		case CodeFileData:
			copied, err := io.CopyN(w, r, payload)
			h.res.BytesReceived += copied
			be.fileBytes += copied
			if h.o.Progress != nil {
				h.o.Progress(h.res.BytesReceived)
			}
			if err != nil {
				return be, err
			}
			last = code
		case CodeSuccess:
			_, err := io.CopyN(io.Discard, r, payload)
			return be, err
		case CodeErrorRemote, CodeErrorLocal:
			// Keep at most maxName bytes of the device-supplied message and skip the rest.
			msg := make([]byte, min(payload, maxName))
			if _, err := io.ReadFull(r, msg); err != nil {
				return be, err
			}
			if _, err := io.CopyN(io.Discard, r, payload-int64(len(msg))); err != nil {
				return be, err
			}
			if last != CodeFileData {
				h.res.RemoteErrorCount++
				h.res.RemoteErrors = appendCapped(h.res.RemoteErrors, fmt.Sprintf("%s: %s", fname, msg))
			} else {
				be.afterData, be.code, be.msg = true, code, string(msg)
			}
			return be, nil
		default:
			return be, fmt.Errorf("unknown block code 0x%02x for %s", code, fname)
		}
	}
}

// noteEnd records or clears the remote end of the file stored at rel. Every
// error-ended file is recorded (the flag set is complete); only the message is
// dropped, past MaxRecordedErrors entries, and that is counted.
func (h *handler) noteEnd(rel string, be blockEnd) {
	if !be.afterData {
		delete(h.res.RemoteEnded, rel) // a later transfer with a success block replaced the content
		return
	}
	if h.res.RemoteEnded == nil {
		h.res.RemoteEnded = map[string]RemoteEnd{}
	}
	e := RemoteEnd{Code: be.code, Bytes: be.fileBytes}
	if h.detailed < MaxRecordedErrors {
		e.Message = be.msg
		h.detailed++
	} else {
		h.res.RemoteEndedDetailOmitted++
	}
	h.res.RemoteEnded[rel] = e
}

// relKey is the key of a device path in Result.RemoteEnded: the cleaned,
// slash-separated path relative to Dir, as receiveFiles records it.
func relKey(p string) string { return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))) }

func underKey(k, root string) bool { return k == root || strings.HasPrefix(k, root+"/") }

// moveEnds carries the marks of from, and of everything below it, to to.
func (h *handler) moveEnds(from, to string) {
	if from == to {
		return
	}
	moved := map[string]RemoteEnd{}
	for k, e := range h.res.RemoteEnded {
		if underKey(k, from) {
			moved[to+k[len(from):]] = e
			delete(h.res.RemoteEnded, k)
		}
	}
	maps.Copy(h.res.RemoteEnded, moved)
}

// copyEnds marks the copies of marked files.
func (h *handler) copyEnds(from, to string) {
	copies := map[string]RemoteEnd{}
	for k, e := range h.res.RemoteEnded {
		if underKey(k, from) {
			copies[to+k[len(from):]] = e
		}
	}
	maps.Copy(h.res.RemoteEnded, copies)
}

// dropEnds is called when root is removed or its content replaced: the marks
// under it leave RemoteEnded for the audit record in RemovedEnds.
func (h *handler) dropEnds(root string) {
	for k, e := range h.res.RemoteEnded {
		if underKey(k, root) {
			delete(h.res.RemoteEnded, k)
			h.res.RemovedEndCount++
			h.res.RemovedEnds = appendCapped(h.res.RemovedEnds, fmt.Sprintf("%s: code 0x%02x, %d bytes", k, e.Code, e.Bytes))
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
		h.dropEnds(relKey(to)) // the destination's old content is gone
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, err)
			continue
		}
		h.moveEnds(relKey(from), relKey(to))
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
			continue
		}
		h.dropEnds(relKey(p))
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
		// Copying a tree into itself would never terminate.
		if insideOrSame(src, dst, foldCase) {
			err = fmt.Errorf("cannot copy %q into itself (%q)", from, to)
		}
	}
	if err == nil {
		err = copyPath(src, dst)
	}
	if err == nil {
		h.dropEnds(relKey(to)) // the copy overwrote whatever was there
		h.copyEnds(relKey(from), relKey(to))
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

// appendCapped appends s unless list already holds MaxRecordedErrors entries.
func appendCapped(list []string, s string) []string {
	if len(list) >= MaxRecordedErrors {
		return list
	}
	return append(list, s)
}

// foldCase is true where the default file system ignores case.
var foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// insideOrSame reports whether dst is src or lies below it, comparing
// cleaned paths and ignoring case when fold is set.
func insideOrSame(src, dst string, fold bool) bool {
	src, dst = filepath.Clean(src), filepath.Clean(dst)
	if fold {
		src, dst = strings.ToLower(src), strings.ToLower(dst)
	}
	return dst == src || strings.HasPrefix(dst, strings.TrimSuffix(src, string(filepath.Separator))+string(filepath.Separator))
}
