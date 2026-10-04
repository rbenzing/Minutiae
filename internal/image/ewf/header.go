package ewf

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var errHeaderTooLarge = errors.New("decompressed text exceeds the 1 MiB limit")

// headerInfo is the "main" category of a header or header2 text: parallel
// field names and values.
type headerInfo struct{ names, values []string }

// inflateCapped inflates a zlib stream, failing when the output would exceed
// limit bytes. Reading to EOF verifies the zlib trailer.
func inflateCapped(p []byte, limit int) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > limit {
		return nil, errHeaderTooLarge
	}
	return out, nil
}

// decodeText turns header bytes into a string with "\n" line ends. wide
// selects UTF-16 (little-endian unless a FE FF byte order mark says
// otherwise); otherwise the bytes are UTF-8 when valid, else Latin-1.
func decodeText(raw []byte, wide bool) string {
	var s string
	switch {
	case wide:
		be := len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff
		if len(raw) >= 2 && (be || (raw[0] == 0xff && raw[1] == 0xfe)) {
			raw = raw[2:]
		}
		u := make([]uint16, len(raw)/2)
		for i := range u {
			if be {
				u[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
			} else {
				u[i] = uint16(raw[2*i+1])<<8 | uint16(raw[2*i])
			}
		}
		s = string(utf16.Decode(u))
	case utf8.Valid(raw):
		s = string(raw)
	default:
		r := make([]rune, len(raw))
		for i, b := range raw {
			r[i] = rune(b)
		}
		s = string(r)
	}
	s = strings.TrimPrefix(s, string(rune(0xFEFF)))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// parseMain finds the first "main" category: a line "main", then a
// tab-separated line of field names and one of values.
func parseMain(text string) (*headerInfo, bool) {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "main" || i+2 >= len(lines) {
			continue
		}
		names := strings.Split(lines[i+1], "\t")
		values := strings.Split(lines[i+2], "\t")
		n := min(len(names), len(values), maxHeaderFields)
		return &headerInfo{names: names[:n], values: values[:n]}, true
	}
	return nil, false
}

// header handles a header or header2 section.
func (o *opener) header(n int, s Segment, sec section) error {
	idx := 0
	if sec.kind == kHeader2 {
		idx = 1
	}
	buf, _, err := readPayload(n, s, sec, 0, maxHeaderZ)
	if err != nil {
		return err
	}
	raw, err := inflateCapped(buf, maxHeaderText)
	if err != nil {
		o.r.warn.add("segment %d: %s section at offset %d cannot be decoded: %v", n, sec.label(), sec.off, err)
		return nil
	}
	if o.hdrRaw[idx] != nil {
		if !bytes.Equal(o.hdrRaw[idx], raw) {
			o.r.warn.add("segment %d: second %s section at offset %d differs from the first", n, sec.label(), sec.off)
		}
		return nil
	}
	info, ok := parseMain(decodeText(raw, idx == 1))
	if !ok {
		o.r.warn.add("segment %d: %s section has no main category", n, sec.label())
		info = &headerInfo{}
	}
	o.hdrRaw[idx] = raw
	o.hdr[idx] = info
	return nil
}

// fieldSet is an insertion-ordered name -> value map.
type fieldSet struct {
	keys []string
	vals map[string]string
}

func (f *fieldSet) set(k, v string, override bool) {
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	old, seen := f.vals[k]
	switch {
	case !seen:
		f.keys = append(f.keys, k)
		f.vals[k] = v
	case override && v != "":
		f.vals[k] = v
	case old == "":
		f.vals[k] = v
	}
}

// knownHeaderFields maps the header field names we name explicitly; the order
// is the Metadata order.
var knownHeaderFields = []struct{ field, key string }{
	{"c", "case_number"},
	{"n", "evidence_number"},
	{"a", "description"},
	{"e", "examiner"},
	{"t", "notes"},
	{"m", "acquired"},
	{"u", "system_date"},
}

func compressionName(level uint8) string {
	switch level {
	case 0:
		return "none"
	case 1:
		return "fast"
	case 2:
		return "best"
	}
	return "unknown (" + strconv.Itoa(int(level)) + ")"
}

func mediaName(t uint8) string {
	switch t {
	case 0:
		return "removable"
	case 1:
		return "fixed"
	case 3:
		return "optical"
	case 0x0e:
		return "logical"
	case 0x10:
		return "memory"
	}
	return fmt.Sprintf("unknown (%#02x)", t)
}

func safeKey(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && i < 32; i++ {
		c := s[i]
		if c > 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// metadata assembles the ordered key/value list.
func (o *opener) metadata() []KV {
	r := o.r
	var fs fieldSet
	for i, h := range o.hdr { // header first, then header2 overrides
		if h == nil {
			continue
		}
		for j, name := range h.names {
			fs.set(name, h.values[j], i == 1)
		}
	}
	var m []KV
	add := func(k, v string) { m = append(m, KV{k, capValue(v)}) }
	add("format", "EWF v1")
	add("segments", strconv.Itoa(len(r.segs)))
	for i, s := range r.segs {
		add("segment."+strconv.Itoa(i+1), s.Name)
	}
	used := map[string]bool{}
	for _, k := range knownHeaderFields {
		used[k.field] = true
		if v := fs.vals[k.field]; v != "" {
			add(k.key, v)
		}
	}
	g := r.geo
	add("compression", compressionName(g.compression))
	add("media", mediaName(g.mediaType))
	add("sectors_per_chunk", strconv.FormatUint(uint64(g.spc), 10))
	add("bytes_per_sector", strconv.FormatUint(uint64(g.bps), 10))
	add("sector_count", strconv.FormatUint(g.sectors, 10))
	add("chunks", strconv.FormatUint(uint64(g.chunks), 10))
	if r.md5 != "" {
		add("md5", r.md5)
	}
	if r.sha1 != "" {
		add("sha1", r.sha1)
	}
	if o.err2 != nil {
		add("acquisition_errors", o.err2.describe())
	}
	extras := 0
	for _, k := range fs.keys {
		if used[k] || k == "" || fs.vals[k] == "" || extras >= maxHeaderFields {
			continue
		}
		extras++
		add("header."+safeKey(k), fs.vals[k])
	}
	if o.unknownCount > 0 {
		add("unknown_sections", strings.Join(o.unknown, ","))
	}
	return m
}
