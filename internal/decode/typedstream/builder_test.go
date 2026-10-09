package typedstream

// buildStream writes a stream the way the format description lays one out: header, an
// NSAttributedString class chain, the NSString class record, the "+" type encoding, the
// length, the text and a stub of an attribute-run section. It is written from the format
// description, not from the extractor, and no real attributedBody blob backs it.

type builderOpts struct {
	mutable bool
	padding int
	rawLen  []byte // replaces the length bytes
	noText  bool   // an NSNumber stub instead of the NSString object
}

type buildOpt func(*builderOpts)

func mutable() buildOpt         { return func(o *builderOpts) { o.mutable = true } }
func padding(n int) buildOpt    { return func(o *builderOpts) { o.padding = n } }
func rawLen(b ...byte) buildOpt { return func(o *builderOpts) { o.rawLen = b } }
func withoutString() buildOpt   { return func(o *builderOpts) { o.noText = true } }

func classRecord(name string) []byte {
	return append([]byte{0x84, byte(len(name))}, name...)
}

func lengthBytes(n int) []byte {
	switch {
	case n < 0x80:
		return []byte{byte(n)}
	case n < 1<<16:
		return []byte{0x81, byte(n), byte(n >> 8)}
	}
	return []byte{0x82, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

func header() []byte {
	return append([]byte{0x04, 0x0B}, "streamtyped\x81\xE8\x03"...)
}

func buildStream(text string, opts ...buildOpt) []byte {
	var o builderOpts
	for _, f := range opts {
		f(&o)
	}
	b := header()
	for i := 0; i < o.padding; i++ {
		b = append(b, 0x00)
	}
	b = append(b, classRecord("NSAttributedString")...)
	b = append(b, classRecord("NSObject")...)
	b = append(b, 0x00, 0x92)
	if o.noText {
		b = append(b, classRecord("NSNumber")...)
		b = append(b, 0x00, 0x01, 'i', 0x07)
		return b
	}
	name := "NSString"
	if o.mutable {
		name = "NSMutableString"
	}
	b = append(b, classRecord(name)...)
	b = append(b, 0x01, 0x94)
	b = append(b, classRecord("NSObject")...)
	b = append(b, 0x00, 0x85, 0x84, 0x01, '+')
	if o.rawLen != nil {
		b = append(b, o.rawLen...)
	} else {
		b = append(b, lengthBytes(len(text))...)
	}
	b = append(b, text...)
	// attribute-run stub
	b = append(b, 0x86, 0x84, 0x02, 'i', 'I', 0x01, 0x05, 0x92)
	b = append(b, classRecord("NSDictionary")...)
	b = append(b, 0x00, 0x01, 0x81, 0x02, 0x00)
	return b
}
