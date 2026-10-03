package mb2

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

func TestDeviceErrno(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int64
	}{
		{fs.ErrNotExist, -6},
		{fs.ErrExist, -7},
		{fmt.Errorf("w: %w", syscall.EISDIR), -9},
		{fmt.Errorf("w: %w", syscall.ELOOP), -10},
		{fmt.Errorf("w: %w", syscall.EIO), -11},
		{fmt.Errorf("w: %w", syscall.ENOSPC), -15},
		{fmt.Errorf("other"), -1},
	} {
		if got := deviceErrno(tc.err); got != tc.want {
			t.Errorf("deviceErrno(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	// On Windows ENOTDIR is ERROR_PATH_NOT_FOUND, which is also "not exist" (-6).
	if !errors.Is(syscall.ENOTDIR, fs.ErrNotExist) {
		if got := deviceErrno(fmt.Errorf("w: %w", syscall.ENOTDIR)); got != -8 {
			t.Errorf("deviceErrno(ENOTDIR) = %d, want -8", got)
		}
	}
}
