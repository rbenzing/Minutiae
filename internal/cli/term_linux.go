//go:build linux

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func isTerminalFile(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

func platformConsoleUTF8() (utf8, ok bool) { return false, false }
