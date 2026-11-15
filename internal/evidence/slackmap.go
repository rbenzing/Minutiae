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
	"unicode/utf8"
)

// The methods of a slack artifact (class slack). File slack is the tail of the last allocation unit of a live
// file; volume slack is what lies in the partition after the end of the filesystem.
const (
	MethodSlackFile   = "slack-file"
	MethodSlackVolume = "slack-volume"
)

// MaxSlackMapLine is the longest line (without its newline) of a slack run map.
const MaxSlackMapLine = 1 << 18

const (
	maxSlackPath = 4 * (32 << 10) // bytes of fs_path: 4 x filesys.MaxWalkPath, the longest escaped label (3F-R28: a backslash-x-HH escape per invalid byte)
	maxSlackID   = 256            // bytes of fs_id
)

// slackMapLineCap bounds the exported lines of a map, and separately its omitted lines. It is a variable only
// so a test can lower it (SetSlackMapLineCap).
var slackMapLineCap = MaxRecoveredRuns

// SlackMapLine is one line of a slack run map ("slack.runs.jsonl", "volume-slack.runs.jsonl"): where the
// segment sits in the artifact (Offset, Length) and in the parent image (ImageOffset). FSPath, PathEscaped,
// FSID and FileSize belong to file lines only (a volume line leaves them zero). FSPath is the escaped label
// when PathEscaped. An omitted line (Exported false) states a uniform range that was left out of the
// artifact: it occupies no artifact bytes, so its Offset is the current one and does not advance.
type SlackMapLine struct {
	Offset, Length, ImageOffset int64
	FSPath, FSID                string
	PathEscaped                 bool
	FileSize                    int64
	Uniform                     bool
	Exported                    bool
}

// slackFileJSON and slackVolumeJSON fix the key order of the written line. The reader decodes into pointer
// copies, so that a missing key is never read as a zero.
type slackFileJSON struct {
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	ImageOffset int64  `json:"image_offset"`
	FSPath      string `json:"fs_path"`
	PathEscaped bool   `json:"path_escaped"`
	FSID        string `json:"fs_id"`
	FileSize    int64  `json:"file_size"`
	Uniform     bool   `json:"uniform"`
	Exported    bool   `json:"exported"`
}

type slackVolumeJSON struct {
	Offset      int64 `json:"offset"`
	Length      int64 `json:"length"`
	ImageOffset int64 `json:"image_offset"`
	Uniform     bool  `json:"uniform"`
	Exported    bool  `json:"exported"`
}

type slackFileIn struct {
	Offset      *int64  `json:"offset"`
	Length      *int64  `json:"length"`
	ImageOffset *int64  `json:"image_offset"`
	FSPath      *string `json:"fs_path"`
	PathEscaped *bool   `json:"path_escaped"`
	FSID        *string `json:"fs_id"`
	FileSize    *int64  `json:"file_size"`
	Uniform     *bool   `json:"uniform"`
	Exported    *bool   `json:"exported"`
}

type slackVolumeIn struct {
	Offset      *int64 `json:"offset"`
	Length      *int64 `json:"length"`
	ImageOffset *int64 `json:"image_offset"`
	Uniform     *bool  `json:"uniform"`
	Exported    *bool  `json:"exported"`
}

var (
	slackFileKeys   = []string{"offset", "length", "image_offset", "fs_path", "path_escaped", "fs_id", "file_size", "uniform", "exported"}
	slackVolumeKeys = []string{"offset", "length", "image_offset", "uniform", "exported"}
)

func slackBad(format string, a ...any) error {
	return fmt.Errorf("%w: slack run map: %s", ErrIntegrity, fmt.Sprintf(format, a...))
}

func slackIsFile(method string) (file bool, err error) {
	switch method {
	case MethodSlackFile:
		return true, nil
	case MethodSlackVolume:
		return false, nil
	}
	return false, slackBad("method %q is not a slack method", method)
}

// checkSlackLine applies to one line, apart from where it sits in the map, every rule the reader and the
// writer share.
func checkSlackLine(file bool, l SlackMapLine) error {
	switch {
	case l.Length <= 0:
		return fmt.Errorf("length %d is not positive", l.Length)
	case l.Offset < 0:
		return errors.New("negative offset")
	case l.ImageOffset < 0:
		return errors.New("negative image_offset")
	case l.Offset > math.MaxInt64-l.Length || l.ImageOffset > math.MaxInt64-l.Length:
		return errors.New("offset or image_offset plus length overflows")
	case !l.Exported && !l.Uniform:
		return errors.New("an omitted range (exported false) must be uniform")
	}
	if !file {
		if l.FSPath != "" || l.FSID != "" || l.FileSize != 0 || l.PathEscaped {
			return errors.New("a volume line has no fs_path, path_escaped, fs_id or file_size")
		}
		return nil
	}
	switch {
	case !utf8.ValidString(l.FSPath) || strings.ContainsRune(l.FSPath, 0):
		return errors.New("fs_path is not valid UTF-8 without NUL")
	case len(l.FSPath) > maxSlackPath:
		return fmt.Errorf("fs_path is longer than %d bytes", maxSlackPath)
	case len(l.FSID) < 1 || len(l.FSID) > maxSlackID:
		return fmt.Errorf("fs_id is %d bytes, want 1 to %d", len(l.FSID), maxSlackID)
	case l.FileSize < 0:
		return errors.New("negative file_size")
	}
	return nil
}

// WriteSlackMapLine writes l as one line with the exact keys of its method. It refuses a line the reader
// would refuse (the sequence of offsets apart).
func WriteSlackMapLine(w io.Writer, method string, l SlackMapLine) error {
	file, err := slackIsFile(method)
	if err != nil {
		return err
	}
	if err := checkSlackLine(file, l); err != nil {
		return slackBad("%v", err)
	}
	var v any
	if file {
		v = slackFileJSON{l.Offset, l.Length, l.ImageOffset, l.FSPath, l.PathEscaped, l.FSID, l.FileSize, l.Uniform, l.Exported}
	} else {
		v = slackVolumeJSON{l.Offset, l.Length, l.ImageOffset, l.Uniform, l.Exported}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// ReadSlackRunMap reads the run map of a slack artifact of the given method strictly and hands each line to
// visit, keeping none of them: one JSON object per line with exactly the keys of the method (none unknown,
// none missing, none repeated or spelled in another case), at most MaxSlackMapLine bytes of valid UTF-8, at
// most MaxRecoveredRuns exported lines and as many omitted ones, positive lengths, non-negative image
// offsets, no overflow, an fs_path without NUL of at most 128 KiB (an escaped 32 KiB walk path) and an fs_id of 1 to 256 bytes. Offsets
// start at 0 and follow one another over the exported lines without gap or overlap; an omitted line
// (exported false, uniform true) sits at the current offset and does not advance it. An empty map is zero
// lines. Every refusal wraps ErrIntegrity; an error of visit or of r is returned as it is.
func ReadSlackRunMap(r io.Reader, method string, visit func(SlackMapLine) error) error {
	file, err := slackIsFile(method)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, 2*MaxSlackMapLine)
	var next int64
	var exported, omitted int
	for n := 1; ; n++ {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return slackBad("line %d: longer than %d bytes", n, MaxSlackMapLine)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		atEOF := err != nil
		if atEOF && len(line) == 0 {
			return nil
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > MaxSlackMapLine {
			return slackBad("line %d: longer than %d bytes", n, MaxSlackMapLine)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return slackBad("line %d: blank", n)
		}
		if !utf8.Valid(line) {
			return slackBad("line %d: not valid UTF-8", n)
		}
		l, err := decodeSlackLine(line, file)
		if err != nil {
			return slackBad("line %d: %v", n, err)
		}
		if err := checkSlackLine(file, l); err != nil {
			return slackBad("line %d: %v", n, err)
		}
		if l.Offset != next {
			return slackBad("line %d: offset %d, want %d (the lines must follow one another from 0, an omitted range at the current offset)", n, l.Offset, next)
		}
		if l.Exported {
			if exported >= slackMapLineCap {
				return slackBad("line %d: more than %d exported lines", n, slackMapLineCap)
			}
			exported++
			next += l.Length
		} else {
			if omitted >= slackMapLineCap {
				return slackBad("line %d: more than %d omitted ranges", n, slackMapLineCap)
			}
			omitted++
		}
		if err := visit(l); err != nil {
			return err
		}
		if atEOF {
			return nil
		}
	}
}

// decodeSlackLine decodes one line into exactly the keys of its form.
func decodeSlackLine(line []byte, file bool) (SlackMapLine, error) {
	keys := slackVolumeKeys
	if file {
		keys = slackFileKeys
	}
	if err := checkExactKeys(line, keys...); err != nil {
		return SlackMapLine{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var l SlackMapLine
	var missing bool
	if file {
		var in slackFileIn
		if err := dec.Decode(&in); err != nil {
			return l, err
		}
		if in.Offset == nil || in.Length == nil || in.ImageOffset == nil || in.FSPath == nil || in.PathEscaped == nil ||
			in.FSID == nil || in.FileSize == nil || in.Uniform == nil || in.Exported == nil {
			missing = true
		} else {
			l = SlackMapLine{*in.Offset, *in.Length, *in.ImageOffset, *in.FSPath, *in.FSID, *in.PathEscaped, *in.FileSize, *in.Uniform, *in.Exported}
		}
	} else {
		var in slackVolumeIn
		if err := dec.Decode(&in); err != nil {
			return l, err
		}
		if in.Offset == nil || in.Length == nil || in.ImageOffset == nil || in.Uniform == nil || in.Exported == nil {
			missing = true
		} else {
			l = SlackMapLine{Offset: *in.Offset, Length: *in.Length, ImageOffset: *in.ImageOffset, Uniform: *in.Uniform, Exported: *in.Exported}
		}
	}
	if missing {
		return SlackMapLine{}, fmt.Errorf("needs the keys %s, none null", strings.Join(keys, ", "))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SlackMapLine{}, errors.New("more than one JSON value")
	}
	return l, nil
}

// SlackMapRuns returns the image runs {ImageOffset, Length} of the exported lines of a slack run map, in
// order (the omitted ranges occupy no artifact bytes and are not runs of it).
func SlackMapRuns(r io.Reader, method string) ([]Run, error) {
	var runs []Run
	err := ReadSlackRunMap(r, method, func(l SlackMapLine) error {
		if l.Exported {
			runs = append(runs, Run{Offset: l.ImageOffset, Length: l.Length})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// sidecarRuns reads the runs sidecar of rec in the format its kind has: the run map of an unallocated
// export, the slack map of a slack artifact (read under the method its Recovery names) or a plain list of
// runs. Every other artifact's sidecar is the plain list.
func sidecarRuns(rec ManifestRecord, r io.Reader) ([]Run, error) {
	switch rec.Source.Kind {
	case "unallocated":
		m, err := ReadUnallocRunMap(r)
		if err != nil {
			return nil, err
		}
		return UnallocRunsAsRuns(m), nil
	case KindSlack:
		method := ""
		if d := rec.Source.Derived; d != nil && d.Recovery != nil {
			method = d.Recovery.Method
		}
		return SlackMapRuns(r, method)
	}
	return readRunLines(r)
}

// slackAllocProblems is rule R5 for class slack (3F-R1): slack lies in allocated units (file slack) or
// outside the filesystem (volume slack), so no byte of it is free, and volume slack, which the filesystem
// has no state for, holds no allocated byte either.
func slackAllocProblems(method string, a AllocSummary) []string {
	var out []string
	if a.Free > 0 {
		out = append(out, fmt.Sprintf("%d free bytes in a slack artifact (slack lies in allocated units or outside the filesystem)", a.Free))
	}
	if method == MethodSlackVolume && a.Allocated > 0 {
		out = append(out, fmt.Sprintf("%d allocated bytes in a volume-slack artifact", a.Allocated))
	}
	return out
}
