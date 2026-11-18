package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
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

// trimPartialRune drops the end of b that may be a cut character: its continuation bytes and one lead byte. It is
// only applied to a buffer that was cut anyway.
func trimPartialRune(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] >= 0xC0 {
		b = b[:len(b)-1]
	}
	return b
}

// joinText joins text parts with "\n", keeping at most 2*maxText bytes, and reports the full length. The cut is made on
// a character boundary, so the cut itself is never reported as invalid UTF-8.
func joinText(parts [][]byte) (joined []byte, total int64) {
	limit := 2 * maxText
	for i, p := range parts {
		sep := 0
		if i > 0 {
			sep = 1
		}
		total += int64(sep + len(p))
		if len(joined) >= limit {
			continue
		}
		if sep == 1 {
			joined = append(joined, '\n')
		}
		if room := limit - len(joined); len(p) > room {
			p = p[:room]
		}
		joined = append(joined, p...)
	}
	if total > int64(len(joined)) {
		joined = trimPartialRune(joined)
	}
	return joined, total
}
