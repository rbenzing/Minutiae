//go:build darwin || freebsd || netbsd || openbsd

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func isTerminalFile(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

func platformConsoleUTF8() (utf8, ok bool) { return false, false }

func termWidth(f *os.File) (int, bool) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 0, false
	}
	return int(ws.Col), true
}
