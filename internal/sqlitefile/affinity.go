package sqlitefile

import "strings"

// AffinityOf returns the affinity the engine gives a column of the declared
// type, by its five rules applied in order to the upper-cased (ASCII) type
// text: it contains "INT": INTEGER; else "CHAR", "CLOB" or "TEXT": TEXT; else
// "BLOB" or no type at all: BLOB (none); else "REAL", "FLOA" or the first four
// letters of DOUBLE: REAL; else NUMERIC. Pinned against the engine by
// TestAffinityMatchesEngine.
func AffinityOf(declType string) Affinity {
	if declType == "" {
		return AffBlob
	}
	u := asciiUpper(declType)
	switch {
	case strings.Contains(u, "INT"):
		return AffInteger
	case strings.Contains(u, "CHAR"), strings.Contains(u, "CLOB"), strings.Contains(u, "TEXT"):
		return AffText
	case strings.Contains(u, "BLOB"):
		return AffBlob
	case strings.Contains(u, "REAL"), strings.Contains(u, "FLOA"), strings.Contains(u, "DOUB"): //nolint:misspell // the engine's REAL rule
		return AffReal
	}
	return AffNumeric
}

// asciiUpper upper-cases only the ASCII letters (the engine's rule is
// byte-wise; strings.ToUpper would also fold other scripts).
func asciiUpper(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// asciiEqualFold reports whether a and b are equal when only ASCII letters
// are case-folded (the engine's identifier comparison).
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
