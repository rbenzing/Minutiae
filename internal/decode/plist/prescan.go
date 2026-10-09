package plist

import (
	"bytes"
	"fmt"
)

// LooksLikeXML reports whether b starts like an XML document: after an optional UTF-8 BOM and
// ASCII whitespace the first byte is '<'. It decides only which validator runs; it validates
// nothing.
func LooksLikeXML(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return c == '<'
		}
	}
	return false
}

// PrescanXML bounds an XML plist without validating it: a single byte pass that counts element
// opens against l.MaxNodes, tracks the nesting of open tags against l.MaxDepth, charges text,
// CDATA and comment bytes against l.MaxPayload (in total, so a document cannot hide size in
// many small runs) and refuses constructs a plist never needs: a document that is not UTF-8,
// a DOCTYPE with an internal subset or an entity, a processing instruction other than the
// leading XML declaration, a root element that is not <plist>, and anything unterminated.
// The zero Limits means DefaultLimits (a partly filled Limits is used as given); MaxDepth is
// capped at 4096. It is conservative: when in doubt it refuses. Known properties, all in the
// refusing direction or bounded by the caller's input cap: leading whitespace before the XML
// declaration is refused (the declaration is only valid at byte 0, after an optional BOM);
// tag names and attribute bytes are not charged against MaxPayload; indentation whitespace
// inside the root is charged as payload; end tags are not matched to their start tags (the
// decoding library is strict about that).
// Every error wraps ErrMalformed, ErrLimit or ErrUnsupported.
func PrescanXML(b []byte, l Limits) error {
	return guard(func() error {
		_, _, err := prescanCore(b, l)
		return err
	})
}

func xmlSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func allXMLSpace(b []byte) bool {
	for _, c := range b {
		if !xmlSpace(c) {
			return false
		}
	}
	return true
}

func nameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == ':'
}

// checkXMLEncoding refuses what is not UTF-8: a UTF-16 or UTF-32 byte order mark, or a NUL in
// the first four bytes (UTF-16 or UTF-32 without one).
func checkXMLEncoding(b []byte) error {
	if bytes.HasPrefix(b, []byte{0xfe, 0xff}) || bytes.HasPrefix(b, []byte{0xff, 0xfe}) ||
		bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}) {
		return fmt.Errorf("%w: XML in UTF-16 or UTF-32", ErrUnsupported)
	}
	if bytes.IndexByte(b[:min(len(b), 4)], 0) >= 0 {
		return fmt.Errorf("%w: XML in UTF-16 or UTF-32", ErrUnsupported)
	}
	return nil
}

// prescanCore returns the element count and the text, CDATA and comment bytes it counted.
func prescanCore(b []byte, l Limits) (nodes, payload uint64, err error) {
	l = l.effective()
	if err := checkXMLEncoding(b); err != nil {
		return 0, 0, err
	}
	charge := func(n int) error {
		payload = sat(payload, uint64(n))
		if payload > l.MaxPayload {
			return limited("XML text above %d bytes", l.MaxPayload)
		}
		return nil
	}
	i := 0
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		i = 3
	}
	first := i // the XML declaration is allowed only here
	depth := 0
	rootSeen, doctypeSeen := false, false
	for i < len(b) {
		if b[i] != '<' {
			end := len(b)
			if k := bytes.IndexByte(b[i:], '<'); k >= 0 {
				end = i + k
			}
			if depth == 0 {
				if !allXMLSpace(b[i:end]) {
					return 0, 0, malformed("text outside the root element")
				}
			} else if err := charge(end - i); err != nil {
				return 0, 0, err
			}
			i = end
			continue
		}
		rest := b[i:]
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			k := bytes.Index(rest[4:], []byte("-->"))
			if k < 0 {
				return 0, 0, malformed("unterminated comment")
			}
			if err := charge(k); err != nil {
				return 0, 0, err
			}
			i += 4 + k + 3
		case bytes.HasPrefix(rest, []byte("<![CDATA[")):
			if depth == 0 {
				return 0, 0, malformed("CDATA outside the root element")
			}
			k := bytes.Index(rest[9:], []byte("]]>"))
			if k < 0 {
				return 0, 0, malformed("unterminated CDATA")
			}
			if err := charge(k); err != nil {
				return 0, 0, err
			}
			i += 9 + k + 3
		case bytes.HasPrefix(rest, []byte("<?")):
			if i != first || !bytes.HasPrefix(rest, []byte("<?xml")) || len(rest) < 6 || !xmlSpace(rest[5]) {
				return 0, 0, malformed("processing instruction")
			}
			k := bytes.Index(rest[2:], []byte("?>"))
			if k < 0 {
				return 0, 0, malformed("unterminated XML declaration")
			}
			i += 2 + k + 2
		case bytes.HasPrefix(rest, []byte("<!DOCTYPE")):
			if rootSeen || doctypeSeen {
				return 0, 0, malformed("misplaced DOCTYPE")
			}
			doctypeSeen = true
			end, err := scanDoctype(rest)
			if err != nil {
				return 0, 0, err
			}
			i += end
		case bytes.HasPrefix(rest, []byte("<!")):
			return 0, 0, malformed("unknown markup declaration")
		case bytes.HasPrefix(rest, []byte("</")):
			k := bytes.IndexByte(rest, '>')
			if k < 0 {
				return 0, 0, malformed("unterminated end tag")
			}
			if depth == 0 {
				return 0, 0, malformed("end tag without a start tag")
			}
			depth--
			i += k + 1
		default:
			end, name, selfClosing, err := scanStartTag(rest)
			if err != nil {
				return 0, 0, err
			}
			if depth == 0 {
				if rootSeen {
					return 0, 0, malformed("more than one root element")
				}
				if string(name) != "plist" {
					return 0, 0, fmt.Errorf("%w: root element %q is not plist", ErrUnsupported, name[:min(len(name), 32)])
				}
				rootSeen = true
			}
			nodes++
			if nodes > l.MaxNodes {
				return 0, 0, limited("more than %d XML elements", l.MaxNodes)
			}
			if !selfClosing {
				depth++
				if depth > l.MaxDepth {
					return 0, 0, limited("nested deeper than %d", l.MaxDepth)
				}
			}
			i += end
		}
	}
	if depth != 0 {
		return 0, 0, malformed("unterminated element")
	}
	if !rootSeen {
		return 0, 0, malformed("no root element")
	}
	return nodes, payload, nil
}

// scanDoctype returns the length of the DOCTYPE declaration at the start of rest. An internal
// subset ('[' outside quotes) or an entity declaration is refused: a plist never needs one.
func scanDoctype(rest []byte) (int, error) {
	var quote byte
	for j := len("<!DOCTYPE"); j < len(rest); j++ {
		c := rest[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			return 0, fmt.Errorf("%w: DOCTYPE with an internal subset", ErrUnsupported)
		case c == '>':
			if bytes.Contains(rest[:j], []byte("<!ENTITY")) {
				return 0, fmt.Errorf("%w: DOCTYPE with an entity", ErrUnsupported)
			}
			return j + 1, nil
		}
	}
	return 0, malformed("unterminated DOCTYPE")
}

// scanStartTag returns the length of the start tag at the start of rest, its name and whether
// it is self-closing. A '>' inside a quoted attribute value does not end the tag.
func scanStartTag(rest []byte) (end int, name []byte, selfClosing bool, err error) {
	if len(rest) < 2 || !nameStart(rest[1]) {
		return 0, nil, false, malformed("bad start tag")
	}
	j := 1
	for j < len(rest) && !xmlSpace(rest[j]) && rest[j] != '/' && rest[j] != '>' {
		j++
	}
	name = rest[1:j]
	var quote byte
	for ; j < len(rest); j++ {
		c := rest[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j + 1, name, rest[j-1] == '/', nil
		}
	}
	return 0, nil, false, malformed("unterminated start tag")
}
