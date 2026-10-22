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

func termWidth(f *os.File) (int, bool) {
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(windows.Handle(f.Fd()), &info) != nil {
		return 0, false
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	return w, w > 0
}
