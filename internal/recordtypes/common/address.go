package common

import (
	"strings"
	"unicode"
)

// Kinds AddressKind returns.
const (
	AddressPhone     = "phone"
	AddressEmail     = "email"
	AddressShortcode = "shortcode"
	AddressAlnum     = "alnum"
	AddressUnknown   = "unknown"
)

// AddressKind classifies an address string by its shape only (C5): it never
// normalizes or resolves anything.
//
//   - email: exactly one '@', a non-empty local part, a domain with a dot inside
//     it (not first or last), no spaces or control characters;
//   - phone: an optional '+', then only ASCII digits and " -().", with 7 to 15 digits;
//   - shortcode: 3 to 6 ASCII digits and nothing else;
//   - alnum: 1 to 11 characters from [A-Za-z0-9 ] with at least one letter;
//   - unknown: everything else (Unicode digits are not digits here).
func AddressKind(addr string) string {
	switch {
	case isEmail(addr):
		return AddressEmail
	case isPhone(addr):
		return AddressPhone
	case isShortcode(addr):
		return AddressShortcode
	case isAlnum(addr):
		return AddressAlnum
	}
	return AddressUnknown
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func isEmail(a string) bool {
	if strings.Count(a, "@") != 1 {
		return false
	}
	if strings.IndexFunc(a, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	local, domain, _ := strings.Cut(a, "@")
	if local == "" || len(domain) < 3 {
		return false
	}
	dot := strings.Index(domain, ".")
	return dot > 0 && domain[len(domain)-1] != '.'
}

func isPhone(a string) bool {
	s := strings.TrimPrefix(a, "+")
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isASCIIDigit(c):
			digits++
		case c == ' ' || c == '-' || c == '(' || c == ')' || c == '.':
		default:
			return false
		}
	}
	return digits >= 7 && digits <= 15
}

func isShortcode(a string) bool {
	if len(a) < 3 || len(a) > 6 {
		return false
	}
	for i := 0; i < len(a); i++ {
		if !isASCIIDigit(a[i]) {
			return false
		}
	}
	return true
}

func isAlnum(a string) bool {
	if len(a) < 1 || len(a) > 11 {
		return false
	}
	letter := false
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			letter = true
		case isASCIIDigit(c), c == ' ':
		default:
			return false
		}
	}
	return letter
}
