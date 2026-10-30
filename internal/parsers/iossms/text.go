package iossms

import (
	"errors"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/typedstream"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const (
	maxText = 256 << 10 // bytes of any stored text kept; the rest is flagged (D9)
	maxList = 1000      // chat members, chat ids and participants per record
	maxAddr = 1024      // the message contract's limit for a participant address
)

// names lists the column names of specs.
func names(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// markText lists field in raw.text_flags (sorted): its text was altered (invalid UTF-8 or NUL became U+FFFD,
// or it was cut). The original bytes stay in the artifact; nothing is altered without a flag (D12).
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	if slices.Contains(cur, any(field)) {
		return
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// cleanField is common.CleanText for a free-text field that has no flag slot in the typed payload: an
// altered text is listed in raw.text_flags.
func cleanField(raw map[string]any, field string, b []byte) string {
	c := common.CleanText(b, maxText)
	if len(c.Flags) > 0 || c.Truncated {
		markText(raw, field)
	}
	return c.Text
}

// textCell reads a TEXT column through cleanField. A column that holds another storage class or cannot be read
// is named in unknown, never read as empty; NULL and a missing column give no value and no flag. An empty text
// gives "" with ok true.
func textCell(row parse.Row, c rowrun.Cols, col, field string, raw, unknown map[string]any) (string, bool) {
	b, k := rowrun.TextCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, c.At(col), k)
		return "", false
	}
	return cleanField(raw, field, b), true
}

// blobCell reads a BLOB column: a TEXT value in it is a storage-class mismatch, not a blob.
func blobCell(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellValue:
		return nil, rowrun.CellOther
	case rowrun.CellOther:
		if bb, ok := row.Blob(col); ok {
			return bb, rowrun.CellValue
		}
	}
	return b, k
}

// setBody puts a cleaned body into the typed message (spec C4, C6).
func setBody(msg *message.Message, c common.Cleaned) {
	msg.BodyFlags = c.Flags
	if c.Truncated {
		t, n := true, int64(c.TotalBytes)
		msg.BodyTruncated, msg.BodyTotalBytes = &t, &n
	}
	msg.BodyRawB64, msg.BodyRawSHA256 = c.RawB64, c.RawSHA256
	if c.RawB64 != "" || c.RawSHA256 != "" {
		n := int64(c.RawLen)
		msg.BodyRawLen = &n
	}
}

// body is the text of a message and where it came from (4E-R14).
type body struct {
	cleaned common.Cleaned
	source  string // message.TextSource*; "" when there is no body
}

// readBody reads the body: a non-NULL text column wins (CleanText, 256 KiB, text_source "text"); else the text of
// a decodable attributedBody (text_source "attributedBody"). When both exist they are compared byte for byte:
// they differ -> the column is kept and raw.attributed_body_differs is set; an attributedBody that cannot be
// decoded sets raw.attributed_body_status "undecodable" and never gives a body (4E-R14). The memory of the
// extraction is returned to the budget as soon as the text is copied.
func readBody(m parse.MapContext, row parse.Row, c rowrun.Cols, raw, unknown map[string]any) body {
	var out body
	text, tk := rowrun.TextCell(row, c.At("text"))
	rowrun.Flag(unknown, row, "text", c.At("text"), tk)
	blob, bk := blobCell(row, c.At("attributedBody"))
	rowrun.Flag(unknown, row, "attributedBody", c.At("attributedBody"), bk)

	var decoded string
	decodedOK := false
	if bk == rowrun.CellValue {
		var bud *parse.BudgetView
		if m != nil && m.Input() != nil {
			bud = m.Input().Budget
		}
		s, status := decodeBody(blob, bud)
		if status != "" {
			raw["attributed_body_status"] = status
		} else {
			decoded, decodedOK = s, true
		}
		defer func() { bud.Free(int64(len(decoded))) }()
	}
	switch {
	case tk == rowrun.CellValue:
		out.cleaned, out.source = common.CleanText(text, maxText), message.TextSourceText
		if decodedOK && decoded != string(text) {
			raw["attributed_body_differs"] = true
		}
	case decodedOK:
		out.cleaned, out.source = common.CleanText([]byte(decoded), maxText), message.TextSourceAttributedBody
	}
	return out
}

// targetGUID is the GUID a reaction points at (4E-R17): the exact forms p:<decimal>/<GUID>, bp:<GUID> or a bare
// <GUID>; anything else has none.
func targetGUID(s string) string {
	if isGUID(s) {
		return s
	}
	if rest, ok := strings.CutPrefix(s, "bp:"); ok {
		if isGUID(rest) {
			return rest
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(s, "p:"); ok {
		n, g, found := strings.Cut(rest, "/")
		if found && n != "" && isGUID(g) && strings.Trim(n, "0123456789") == "" {
			return g
		}
	}
	return ""
}

// isGUID reports 36 characters of [0-9A-Fa-f-] in the 8-4-4-4-12 layout.
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if ch != '-' {
				return false
			}
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f', ch >= 'A' && ch <= 'F':
		default:
			return false
		}
	}
	return true
}

// decodeBody extracts the text of an attributedBody. status is "" on success; otherwise "undecodable" (the
// bytes are not a readable typedstream), "no-string" (a well-formed stream with no string object) or "budget"
// (the host memory budget refused the extraction: a fact about this run, not about the data).
func decodeBody(blob []byte, bud *parse.BudgetView) (text, status string) {
	s, ok, err := typedstream.ExtractText(blob, bud)
	switch {
	case errors.Is(err, typedstream.ErrNoBudget):
		return "", "budget"
	case err != nil:
		return "", "undecodable"
	case !ok:
		return "", "no-string"
	}
	return s, ""
}
