package androidtelegram

import (
	"bytes"
	"encoding/binary"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The bounds of the trailer (spec 8.8, 12).
const (
	maxAttachPath = 4 << 10 // the attach path, cleaned and flagged
	maxParams     = 64      // the params kept; attach_params_total counts them all
	maxParamText  = 1 << 10 // a param's key or value, cleaned
)

// The trailer states (spec 8.8).
const (
	trailerNone        = "none"        // no byte after the body
	trailerRead        = "read"        // the pinned shape consumed every remaining byte
	trailerUnparsed    = "unparsed"    // bytes that do not form the pinned shape: length and SHA-256 only
	trailerUnreachable = "unreachable" // the body did not decode whole: where it ends is unknown
	attachEmptyMarker  = "empty-marker"
)

// flagFwd is MESSAGE_FLAG_FWD (TLRPC:64): with a negative (local) id, the trailer holds fwd_msg_id after its string.
const flagFwd = 0x4

// readTrailer states the app-local trailer after a message body whose top constructor c was read (spec 6.5, 8.8,
// call 5; TLRPC:59562-59690): only when the body decoded whole (else "unreachable") and bytes remain after it (else
// "none"): one TL string, then an int32 fwd_msg_id iff the decoded flags have 0x4 and the id is negative (the
// non-secret classes; a secret class writes the string alone). It is "read" only when that shape consumes every
// remaining byte, else "unparsed" with trailer_len and trailer_sha256 and nothing of it read (Review Focus 5). The
// string " " is the empty-path marker; a string starting "||" holds key|=|value pairs split on "||" (at most 64 kept,
// keys and values cleaned at 1 KiB, attach_params_total, attach_params_truncated), the rest is the path (cleaned at
// 4 KiB, attach_path_flags). Raw only: never looked up, never a source_ref.
func readTrailer(b []byte, v view, c tl.Constructor, raw map[string]any) {
	if v.r.Stop != nil {
		raw["trailer_state"] = trailerUnreachable
		return
	}
	rest := b[v.r.End:]
	if len(rest) == 0 {
		raw["trailer_state"] = trailerNone
		return
	}
	s, fwdID, ok := trailerShape(rest, v, c)
	if !ok {
		raw["trailer_state"], raw["trailer_len"], raw["trailer_sha256"] = trailerUnparsed, int64(len(rest)), sha256Hex(rest)
		return
	}
	raw["trailer_state"] = trailerRead
	if fwdID != nil {
		raw["fwd_msg_id"] = *fwdID
	}
	if bytes.Equal(s, []byte(" ")) {
		raw["attach_path_state"] = attachEmptyMarker
		return
	}
	path := s
	if after, isParams := bytes.CutPrefix(s, []byte("||")); isParams {
		path = attachParams(raw, after)
	}
	if len(path) > 0 {
		cl := common.CleanText(path, maxAttachPath)
		raw["attach_path"] = cl.Text
		if len(cl.Flags) > 0 {
			raw["attach_path_flags"] = strings.Join(cl.Flags, ",")
		}
	}
}

// trailerShape reads the pinned trailer shape from rest: the string and, when the rule asks for it, fwd_msg_id; ok
// only when the shape consumes every byte of rest.
func trailerShape(rest []byte, v view, c tl.Constructor) (s []byte, fwdID *int64, ok bool) {
	r, err := tl.NewReader(rest)
	if err != nil {
		return nil, nil, false
	}
	if s, err = r.String(); err != nil {
		return nil, nil, false
	}
	flags, flagsOK := v.Int("flags")
	id, idOK := v.Int("id")
	if !strings.HasPrefix(c.Name, "TL_message_secret") && flagsOK && flags&flagFwd != 0 && idOK && id < 0 {
		if r.Remaining() != 4 {
			return nil, nil, false
		}
		x := int64(int32(binary.LittleEndian.Uint32(rest[r.Offset():])))
		fwdID = &x
		return s, fwdID, true
	}
	return s, nil, r.Remaining() == 0
}

// attachParams states the pairs of a trailer string after its "||" (spec 8.8): the pieces split on "||" left to
// right, each but the last a key|=|value pair, the last the path it returns. At most maxParams pairs are kept as
// [{key, value}] in order, keys and values cleaned at 1 KiB; attach_params_total counts them all and
// attach_params_truncated says more were met. The pieces are walked in place: nothing is allocated past the cap.
func attachParams(raw map[string]any, s []byte) []byte {
	sep := []byte("||")
	var list []any
	var total int64
	for {
		p, rest, found := bytes.Cut(s, sep)
		if !found {
			break
		}
		s = rest
		total++
		if len(list) == maxParams {
			raw["attach_params_truncated"] = true
			continue
		}
		k, val, _ := bytes.Cut(p, []byte("|=|"))
		list = append(list, map[string]any{
			"key":   common.CleanText(k, maxParamText).Text,
			"value": common.CleanText(val, maxParamText).Text,
		})
	}
	if total > 0 {
		raw["attach_params"], raw["attach_params_total"] = list, total
	}
	return s
}
