package sqlitefile

import (
	"errors"
	"fmt"
	"io"
)

// readFull reads exactly len(p) bytes at off. The caller has checked that the
// range lies wholly below the authoritative size, so a short read means the
// reader's own size was wrong: that is an I/O error (wrapping
// io.ErrUnexpectedEOF), never corruption. Any other reader error is wrapped
// as it is.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) || n < 0 || n > len(p) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("sqlitefile: reading %d bytes at offset %d: %w", len(p), off, err)
}

// isUnavailable reports whether err says a page cannot be supplied.
func isUnavailable(err error) bool { return errors.Is(err, ErrPageUnavailable) }
