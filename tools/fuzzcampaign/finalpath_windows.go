//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// finalDir returns the final path of the existing directory p as Windows reports it, with
// every symlink and junction followed (filepath.EvalSymlinks does not follow a junction) and
// the extended-length prefix removed.
func finalDir(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("fuzzcampaign: cannot open %s to find its final path: %w", p, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_PATH+1)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", fmt.Errorf("fuzzcampaign: final path of %s: %w", p, err)
		}
		if int(n) < len(buf) {
			return stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}

var (
	extPrefix    = strings.Repeat(string(filepath.Separator), 2) + "?" + string(filepath.Separator)
	extUNCPrefix = extPrefix + "UNC" + string(filepath.Separator)
)

func stripExtendedPrefix(s string) string {
	switch {
	case strings.HasPrefix(s, extUNCPrefix):
		return strings.Repeat(string(filepath.Separator), 2) + s[len(extUNCPrefix):]
	case strings.HasPrefix(s, extPrefix):
		return s[len(extPrefix):]
	}
	return s
}
