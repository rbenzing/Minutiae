package plist

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The decoding library turns an XML character reference to a surrogate (&#xD800;) into
// U+FFFD, which cannot be told from a real U+FFFD. Decode therefore rewrites such references
// to references to private code points of plane 16 before the library sees the document, and
// maps them back to the surrogates afterwards. The block of stand-ins is chosen so that no
// character or reference in the document already uses it.

const (
	standBlocks = 8
	standSize   = 2048
	standTop    = 0x10F000 // blocks start at standTop - k*standSize
)

// scanRefs calls fn for every numeric character reference (&#123; or &#x1F;) outside CDATA
// sections and comments, with the byte range of the reference and its value (saturated at
// 0x110000).
func scanRefs(b []byte, fn func(start, end int, val uint32)) {
	for i := 0; i < len(b); {
		k := bytes.IndexAny(b[i:], "&<")
		if k < 0 {
			return
		}
		i += k
		if b[i] == '<' {
			switch rest := b[i:]; {
			case bytes.HasPrefix(rest, []byte("<![CDATA[")):
				if e := bytes.Index(rest, []byte("]]>")); e >= 0 {
					i += e + 3
				} else {
					return
				}
			case bytes.HasPrefix(rest, []byte("<!--")):
				if e := bytes.Index(rest, []byte("-->")); e >= 0 {
					i += e + 3
				} else {
					return
				}
			default:
				i++
			}
			continue
		}
		if end, val, ok := parseRef(b, i); ok {
			fn(i, end, val)
			i = end
		} else {
			i++
		}
	}
}

// parseRef parses a numeric character reference at b[i] ('&'). Like the XML decoder it
// accepts a lowercase x only.
func parseRef(b []byte, i int) (end int, val uint32, ok bool) {
	j := i + 1
	if j >= len(b) || b[j] != '#' {
		return 0, 0, false
	}
	j++
	base := uint32(10)
	if j < len(b) && b[j] == 'x' {
		base, j = 16, j+1
	}
	start := j
	for ; j < len(b); j++ {
		d, ok := digit(b[j], base)
		if !ok {
			break
		}
		if val <= 0x110000 {
			val = val*base + d
		}
	}
	if j == start || j >= len(b) || b[j] != ';' {
		return 0, 0, false
	}
	return j + 1, min(val, 0x110000), true
}

func digit(c byte, base uint32) (uint32, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint32(c - '0'), true
	case base == 16 && c >= 'a' && c <= 'f':
		return uint32(c-'a') + 10, true
	case base == 16 && c >= 'A' && c <= 'F':
		return uint32(c-'A') + 10, true
	}
	return 0, false
}

func isSurrogate(v uint32) bool { return v >= 0xD800 && v < 0xE000 }

// rewriteSurrogates returns a copy of the XML document b with every character reference to
// a surrogate replaced by a reference to a stand-in code point, and the first code point of
// the block of stand-ins. found is false (and nothing is copied) when there is no such
// reference. ok is false when no block of stand-ins is free of the document's own characters.
func rewriteSurrogates(b []byte) (out []byte, base rune, found, ok bool) {
	var used [standBlocks]bool
	mark := func(r rune) {
		if r >= standTop-(standBlocks-1)*standSize && r < standTop+standSize {
			used[(standTop+standSize-1-int(r))/standSize] = true
		}
	}
	scanRefs(b, func(_, _ int, v uint32) {
		if isSurrogate(v) {
			found = true
		} else {
			mark(rune(v))
		}
	})
	if !found {
		return nil, 0, false, true
	}
	// Raw characters in the range: UTF-8 sequences F4 8x .. (U+10x000), found by their lead.
	for i := 0; i < len(b); {
		k := bytes.IndexByte(b[i:], 0xF4)
		if k < 0 {
			break
		}
		i += k
		if r, n := utf8.DecodeRune(b[i:]); n == 4 {
			mark(r)
		}
		i++
	}
	free := -1
	for k := range used {
		if !used[k] {
			free = k
			break
		}
	}
	if free < 0 {
		return nil, 0, true, false
	}
	base = rune(standTop - free*standSize)
	out = make([]byte, 0, len(b))
	last := 0
	scanRefs(b, func(s, e int, v uint32) {
		if isSurrogate(v) {
			out = append(out, b[last:s]...)
			out = append(out, "&#x"...)
			out = strconv.AppendUint(out, uint64(base)+uint64(v-0xD800), 16)
			out = append(out, ';')
			last = e
		}
	})
	out = append(out, b[last:]...)
	return out, base, true, true
}

// checkXMLStructure reads the XML document b with encoding/xml tokens (entities, comments,
// CDATA, whitespace and attributes do not matter) and refuses what the decoding library would
// read as something else than the document says:
//
//   - a CF$UID key whose next element is an integer with a negative value (the library reads
//     it as a huge unsigned UID);
//   - any child element inside <key> or <integer> (the library skips the child and keeps the
//     direct text, so the value is not what the document shows);
//   - a <key> directly after a <key> in a <dict> (the library keeps only the second).
//
// The document has passed the pre-scan, so its size, depth and element count are bounded; a
// token error ends the scan (the library reports it).
func checkXMLStructure(b []byte) error {
	type frame struct {
		name       string
		pendingKey bool // a dict frame: its last element was a <key>
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	var stack []frame
	var text strings.Builder
	const (
		none = iota
		inKey
		afterUIDKey
		inInteger
	)
	state := none
	for {
		tok, err := d.Token()
		if err != nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := elementKind(t.Name)
			if n := len(stack); n > 0 {
				switch parent := &stack[n-1]; parent.name {
				case "key", "integer":
					return malformed("element inside <%s>", parent.name)
				case "dict":
					if name == "key" && parent.pendingKey {
						return malformed("two <key> elements in a row")
					}
					parent.pendingKey = name == "key"
				}
			}
			stack = append(stack, frame{name: name})
			switch {
			case name == "key":
				state = inKey
			case name == "integer" && state == afterUIDKey:
				state = inInteger
			default:
				state = none
			}
			text.Reset()
		case xml.CharData:
			if state == inKey || state == inInteger {
				text.Write(t)
			}
		case xml.EndElement:
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
			switch {
			case state == inKey && elementKind(t.Name) == "key" && text.String() == "CF$UID":
				state = afterUIDKey
			case state == inInteger:
				if strings.HasPrefix(strings.TrimSpace(text.String()), "-") {
					return malformed("negative CF$UID")
				}
				state = none
			default:
				state = none
			}
			text.Reset()
		}
	}
}

// elementKind returns "key", "integer" or "dict" for an element of that local name in any
// namespace (the library reads elements by local name) and "" for every other element.
func elementKind(n xml.Name) string {
	n.Space = ""
	for _, k := range [...]string{"key", "integer", "dict"} {
		if n == (xml.Name{Local: k}) {
			return k
		}
	}
	return ""
}
