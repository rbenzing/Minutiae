package androidgooglemessages

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The bounds of the mappings (spec 3, 9): the telephony parsers' caps and the limit of an identifier.
const (
	maxBody  = 256 << 10 // bytes of body text kept; the rest is flagged
	maxList  = 1000      // participants, attachments, thread members, parts per record
	maxIdent = 1 << 10   // an identifier is never cut: over this it is absent with its length and SHA-256
	maxShort = 4 << 10   // uri, content_type, mms_content_location, mms_transaction_id
)

// sha256Hex is the SHA-256 of b in lower-case hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// markText lists field in raw.text_flags (sorted): the text of that field was altered (invalid UTF-8 or NUL became U+FFFD,
// or it was cut). The original bytes stay in the artifact; nothing is altered without a flag.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// cleanField is common.CleanText for a free-text field that has no flag slot of its own in the typed payload; an altered
// text is listed in raw.text_flags.
func cleanField(raw map[string]any, key string, b []byte, limit int) string {
	cl := common.CleanText(b, limit)
	if len(cl.Flags) > 0 {
		markText(raw, key)
	}
	return cl.Text
}

// ident cleans an identifier that must never be cut (spec 9): over limit bytes, as stored or once each invalid byte is
// U+FFFD, it is not kept: raw[key+"_state"] = "too-long" with the stored length and SHA-256. ok is false then.
func ident(raw map[string]any, key string, b []byte, limit int) (string, bool) {
	if len(b) <= limit {
		if cl := common.CleanText(b, 3*limit); len(cl.Text) <= limit {
			if len(cl.Flags) > 0 {
				markText(raw, key)
			}
			return cl.Text, true
		}
	}
	raw[key+"_state"] = "too-long"
	raw[key+"_len"] = int64(len(b))
	raw[key+"_sha256"] = sha256Hex(b)
	return "", false
}

// nanCell says a cell is a stored REAL NaN: the reader (as the engine) reads it as NULL but keeps the serial type 7, so it
// is detectable here and is never read as absent (unknown is never absent, 4I2-A6). Restated from androidcalendar (a
// parser imports no other parser).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// intCellOf is rowrun.IntCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func intCellOf(row parse.Row, col int) (int64, rowrun.CellKind) {
	v, k := rowrun.IntCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return v, k
}

// textCellOf is rowrun.TextCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func textCellOf(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	b, k := rowrun.TextCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return b, k
}
