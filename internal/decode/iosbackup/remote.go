package iosbackup

import (
	"strings"
	"unicode/utf8"
)

// ValidFileID reports whether id is 40 lowercase hexadecimal digits, the form of a backup file ID
// (the hex SHA-1 of "<domain>-<relativePath>").
func ValidFileID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SplitRemote splits a backup file's remote path, exactly "<dir>/<aa>/<fileID>" with a non-empty
// dir that holds no separator, a valid file ID and aa equal to its first two digits. Anything else
// (extra elements, an empty element, upper-case digits, a prefix that disagrees) is not ok.
func SplitRemote(remote string) (dir, fileID string, ok bool) {
	parts := strings.Split(remote, "/")
	if len(parts) != 3 || parts[0] == "" || !ValidFileID(parts[2]) || parts[1] != parts[2][:2] {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// maxDomainBytes bounds a backup domain (4E-R2).
const maxDomainBytes = 255

// ValidDomain reports whether d is usable as the domain of a logical path (4E-R2): non-empty, at
// most 255 bytes, valid UTF-8, without "/", a backslash or NUL, and not "." or "..". It never
// repairs d.
func ValidDomain(d string) bool {
	if d == "" || len(d) > maxDomainBytes || d == "." || d == ".." || !utf8.ValidString(d) {
		return false
	}
	return !strings.ContainsAny(d, "/\\\x00")
}

// ValidRelativePath reports whether p is canonical exactly as stored (4E-R42): non-empty, valid
// UTF-8, no NUL or backslash, no leading or trailing "/", no empty ("//") element and no "." or
// ".." element. It never repairs p: a path that fails is refused, not collapsed.
func ValidRelativePath(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for el := range strings.SplitSeq(p, "/") {
		if el == "" || el == "." || el == ".." {
			return false
		}
	}
	return true
}
