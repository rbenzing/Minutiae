package dbrecover

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

const (
	hexUpper = "0123456789ABCDEF"
)

// pctEncode percent-encodes every byte outside [A-Za-z0-9._~-] as uppercase %XX.
func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&15])
	}
	return b.String()
}

// locatorName is the encoded form of a name, or "" when it is not
// representable (empty, invalid UTF-8, NUL). A name over LocatorNameBytes is cut
// on a rune boundary and followed by "~" and the first 8 hex digits of the
// SHA-256 of the whole name.
func locatorName(name string) string {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return ""
	}
	if len(name) > LocatorNameBytes {
		cut := LocatorNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		sum := sha256.Sum256([]byte(name))
		name = name[:cut] + "~" + hex.EncodeToString(sum[:4])
	}
	return pctEncode(name)
}

// locatorFor is the locator of a recovered row:
// sqlite:[table=|index=<name>;]page=<n>;(cell=<i>|span=<off>+<len>)[;wal_frame=|freelist_page=|journal_rec=<n>];file=db|wal|journal
func locatorFor(r sqlitefile.RecoveredRow) string {
	var b strings.Builder
	b.WriteString("sqlite:")
	name, key := locatorName(r.Table), "table="
	if r.Table == "" {
		name, key = locatorName(r.Index), "index="
	}
	if name != "" {
		b.WriteString(key + name + ";")
	}
	b.WriteString("page=" + strconv.FormatUint(uint64(r.Loc.Page), 10))
	if r.Loc.Cell < 0 {
		b.WriteString(";span=" + strconv.FormatInt(r.Loc.Offset-r.Loc.PageOffset, 10) + "+" + strconv.FormatInt(r.Loc.Length, 10))
	} else {
		b.WriteString(";cell=" + strconv.Itoa(r.Loc.Cell))
	}
	switch {
	case r.Loc.File == sqlitefile.FileWAL:
		b.WriteString(";wal_frame=" + strconv.FormatUint(uint64(r.Loc.Frame), 10))
	case r.Loc.File == sqlitefile.FileJournal:
		b.WriteString(";journal_rec=" + strconv.Itoa(r.Loc.Record))
	case r.Origin == sqlitefile.OriginFreelistLeaf || r.Origin == sqlitefile.OriginFreelistTrunk:
		b.WriteString(";freelist_page=" + strconv.FormatUint(uint64(r.Loc.Page), 10))
	}
	switch r.Loc.File {
	case sqlitefile.FileWAL:
		b.WriteString(";file=wal")
	case sqlitefile.FileJournal:
		b.WriteString(";file=journal")
	default:
		b.WriteString(";file=db")
	}
	return b.String()
}
