//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package cli

import "os"

func isTerminalFile(*os.File) bool { return false }

func platformConsoleUTF8() (utf8, ok bool) { return false, false }

func termWidth(*os.File) (int, bool) { return 0, false }
