//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func isTerminalFile(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

func platformConsoleUTF8() (utf8, ok bool) {
	cp, err := windows.GetConsoleOutputCP()
	return err == nil && cp == 65001, true
}
