package evidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// UnallocRun is one line of an unallocated export's run map ("unallocated.runs.jsonl"): where the run
// sits in the export (Offset, Length) and in the parent image (ImageOffset). It is not an evidence.Run.
type UnallocRun struct {
	Offset      int64
	Length      int64
	ImageOffset int64
}

type unallocLineJSON struct {
	Offset      *int64 `json:"offset"`
	Length      *int64 `json:"length"`
	ImageOffset *int64 `json:"image_offset"`
}

func unallocBad(format string, a ...any) error {
	return fmt.Errorf("%w: unallocated run map: %s", ErrIntegrity, fmt.Sprintf(format, a...))
}

// ReadUnallocRunMap reads the run map of an unallocated export strictly: one JSON object per line with
// exactly the keys offset, length and image_offset (no unknown field, none missing), at most
// MaxRecoveredRuns lines of at most 256 bytes, offsets starting at 0 and following one another without gap
// or overlap, positive lengths, non-negative image offsets and no overflow of offset+length or
// image_offset+length. An empty file is zero runs. Every refusal wraps ErrIntegrity.
func ReadUnallocRunMap(r io.Reader) ([]UnallocRun, error) {
	br := bufio.NewReaderSize(r, 2*maxRunsLine)
	var out []UnallocRun
	var next int64
	for n := 1; ; n++ {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, unallocBad("line %d is longer than %d bytes", n, maxRunsLine)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		atEOF := err != nil
		if atEOF && len(line) == 0 {
			return out, nil
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > maxRunsLine {
			return nil, unallocBad("line %d is longer than %d bytes", n, maxRunsLine)
		}
		if len(out) >= MaxRecoveredRuns {
			return nil, unallocBad("more than %d runs", MaxRecoveredRuns)
		}
		if err := checkUnallocKeys(line); err != nil {
			return nil, unallocBad("line %d: %v", n, err)
		}
		var l unallocLineJSON
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return nil, unallocBad("line %d: %v", n, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, unallocBad("line %d: more than one JSON value", n)
		}
		if l.Offset == nil || l.Length == nil || l.ImageOffset == nil {
			return nil, unallocBad("line %d: needs offset, length and image_offset", n)
		}
		u := UnallocRun{*l.Offset, *l.Length, *l.ImageOffset}
		switch {
		case u.Offset != next:
			return nil, unallocBad("line %d: offset %d, want %d (runs must follow one another from 0)", n, u.Offset, next)
		case u.Length <= 0:
			return nil, unallocBad("line %d: length %d is not positive", n, u.Length)
		case u.ImageOffset < 0:
			return nil, unallocBad("line %d: negative image_offset", n)
		case u.Offset > math.MaxInt64-u.Length || u.ImageOffset > math.MaxInt64-u.Length:
			return nil, unallocBad("line %d: offset or image_offset plus length overflows", n)
		}
		next = u.Offset + u.Length
		out = append(out, u)
		if atEOF {
			return out, nil
		}
	}
}

// checkUnallocKeys walks the tokens of one line before it is decoded and refuses what encoding/json would
// quietly accept: a repeated key (the last would win) and a key that differs from the exact lower-case name
// only in case (it would match case-insensitively). Malformed JSON is left to the decoder.
func checkUnallocKeys(line []byte) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := t.(string)
		if !ok {
			return nil
		}
		switch key {
		case "offset", "length", "image_offset":
		default:
			if strings.EqualFold(key, "offset") || strings.EqualFold(key, "length") || strings.EqualFold(key, "image_offset") {
				return fmt.Errorf("key %q is not spelled exactly", key)
			}
			return nil
		}
		if seen[key] {
			return fmt.Errorf("key %q appears twice", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}

// UnallocRunsAsRuns converts a run map to the image runs, in order.
func UnallocRunsAsRuns(m []UnallocRun) []Run {
	out := make([]Run, len(m))
	for i, u := range m {
		out[i] = Run{Offset: u.ImageOffset, Length: u.Length}
	}
	return out
}
