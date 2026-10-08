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
//   - phone: an optional '+', then only ASCII digits and " -().", with 7 to 15 digits, but
//     never an IPv4 address or a separated date (2024-01-15, 15.01.2024), which fit the
//     same shape; compact digit strings (an epoch time, 20240115) stay phones, so the
//     kind is a shape hint and never an identity;
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
	if looksLikeIPOrDate(strings.TrimSpace(a)) {
		return false
	}
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

// looksLikeIPOrDate reports an IPv4 address (four groups of 1 to 3 digits joined by
// dots) or a separated date (three digit groups joined by '-' or by '.', sized
// year-month-day or day-month-year or month-day-year: 4+1..2+1..2, 1..2+1..2+4)
// so the phone shape, which fits both, does not label them. Only plain digit
// groups with one separator kind count: a number with a plus, spaces or
// parentheses is a phone.
func looksLikeIPOrDate(a string) bool {
	sep := byte(0)
	for i := 0; i < len(a); i++ {
		switch c := a[i]; {
		case isASCIIDigit(c):
		case (c == '.' || c == '-') && (sep == 0 || sep == c):
			sep = c
		default:
			return false
		}
	}
	if sep == 0 {
		return false
	}
	groups := strings.Split(a, string(sep))
	for _, g := range groups {
		if g == "" {
			return false
		}
	}
	n := func(i int) int { return len(groups[i]) }
	switch len(groups) {
	case 4:
		return sep == '.' && n(0) <= 3 && n(1) <= 3 && n(2) <= 3 && n(3) <= 3
	case 3:
		yearFirst := n(0) == 4 && n(1) <= 2 && n(2) <= 2
		yearLast := n(0) <= 2 && n(1) <= 2 && n(2) == 4
		return yearFirst || yearLast
	}
	return false
}
