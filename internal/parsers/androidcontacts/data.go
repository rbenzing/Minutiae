package androidcontacts

import (
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

const (
	maxText  = 256 << 10
	maxList  = 1000
	maxNotes = 100
)

// The platform's mimetypes, as the suffix after "vnd.android.cursor.item/".
const mimePrefix = "vnd.android.cursor.item/"

func dataNames() []string {
	names := []string{"mimetype_id", "is_primary"}
	for i := 1; i <= 15; i++ {
		names = append(names, "data"+strconv.Itoa(i))
	}
	return names
}

// numCell reads a number the platform stores in a TEXT-affinity dataN column: an
// integer, or decimal text.
func numCell(row parse.Row, col int) (int64, bool) {
	if v, k := rowrun.IntCell(row, col); k == rowrun.CellValue {
		return v, true
	}
	if b, k := rowrun.TextCell(row, col); k == rowrun.CellValue && len(b) <= 32 { // longer text is no number: refused before any conversion (D15)
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// markText lists field in raw.text_flags (sorted): the text of that field was altered
// (invalid UTF-8 or NUL became U+FFFD, or it was cut). The original bytes stay in the
// artifact; nothing is altered without a flag (spec 7.6).
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

// cleanField is common.CleanText for a free-text field that has no flag slot of its own
// in the typed payload; an altered text is listed in raw.text_flags.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}

// dataRow reads the cells of one data row; text that had to be altered is listed in raw.text_flags as "data".
type dataRow struct {
	row parse.Row
	c   rowrun.Cols
	raw map[string]any
}

func (d dataRow) text(n int) string {
	b, k := rowrun.TextCell(d.row, d.c.At("data"+strconv.Itoa(n)))
	if k != rowrun.CellValue {
		return ""
	}
	return cleanField(d.raw, "data", b)
}

func (d dataRow) num(n int) (int64, bool) { return numCell(d.row, d.c.At("data"+strconv.Itoa(n))) }

func (d dataRow) primary() *bool {
	if v, k := rowrun.IntCell(d.row, d.c.At("is_primary")); k == rowrun.CellValue && (v == 0 || v == 1) {
		b := v == 1
		return &b
	}
	return nil
}

// labelOf names a typed value: type 0 is the custom text, a known type its name, an
// unknown type no label (and unknown reports it); a NULL type has no label either.
func labelOf(t int64, ok bool, custom string, name func(int64) string) (label string, unknown bool) {
	switch {
	case !ok:
		return "", false
	case t == 0:
		return custom, false
	}
	if n := name(t); n != "" {
		return n, false
	}
	return "", true
}

func phoneName(v int64) string {
	switch v {
	case 1:
		return "home"
	case 2:
		return "mobile"
	case 3:
		return "work"
	case 4:
		return "fax_work"
	case 5:
		return "fax_home"
	case 6:
		return "pager"
	case 7:
		return "other"
	}
	return ""
}

func emailName(v int64) string {
	switch v {
	case 1:
		return "home"
	case 2:
		return "work"
	case 3:
		return "other"
	case 4:
		return "mobile"
	}
	return ""
}

func urlName(v int64) string {
	switch v {
	case 1:
		return "homepage"
	case 2:
		return "blog"
	case 3:
		return "profile"
	case 4:
		return "home"
	case 5:
		return "work"
	case 6:
		return "ftp"
	case 7:
		return "other"
	}
	return ""
}

func imName(v int64) string {
	switch v {
	case 0:
		return "aim"
	case 1:
		return "msn"
	case 2:
		return "yahoo"
	case 3:
		return "skype"
	case 4:
		return "qq"
	case 5:
		return "google_talk"
	case 6:
		return "icq"
	case 7:
		return "jabber"
	case 8:
		return "netmeeting"
	}
	return ""
}

// imLabel names an instant-messaging protocol. Protocol 0 is AIM (the custom protocol is -1,
// which the caller handles); an unrecognised protocol has no label and reports unknown.
func imLabel(p int64, ok bool) (string, bool) {
	if !ok {
		return "", false
	}
	if n := imName(p); n != "" {
		return n, false
	}
	return "", true
}

func postalName(v int64) string {
	switch v {
	case 1:
		return "home"
	case 2:
		return "work"
	case 3:
		return "other"
	}
	return ""
}

func joinSpace(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

func capValues(vs []contact.Value, name string, raw map[string]any) []contact.Value {
	if len(vs) > maxList {
		raw[name+"_total"], raw[name+"_truncated"] = int64(len(vs)), true
		return vs[:maxList]
	}
	return vs
}
