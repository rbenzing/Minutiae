// Package tl reads the binary TL encoding the Telegram Android app stores in its database blobs.
//
// The Reader is schema-less: it reads one primitive at a time from a byte slice and knows no
// constructor. It imports no Minutiae package, never writes anywhere and is never driven by a
// declared size: a length or count read from the blob is checked against the bytes that remain
// (and against a package cap) before it is used, and Bytes and String return sub-slices of the
// input, never a copy, so nothing is ever allocated from a stored length.
//
// Wire facts (plan 4I4 spec section 6.5; the app source at the A-HEAD pin, short paths
// SD = tgnet/SerializedData.java, NBB = tgnet/NativeByteBuffer.java, VEC = tgnet/Vector.java,
// TLO = tgnet/TLObject.java):
//
//   - int32 and int64 are little-endian (SD:121-153, NBB:41,59,415,464).
//   - A double is the IEEE-754 bits of a little-endian int64 (SD:318-327).
//   - A bool is boxed: boolTrue 0x997275b5 or boolFalse 0xbc799737; any other value is an error
//     (NBB:447-462).
//   - A string and bytes share one framing (SD:227-265, NBB:234-312,526-596): a first byte
//     L <= 253 is the length; otherwise the length is the next 3 bytes, little-endian. The
//     app's reader takes any first byte >= 254 as the long form, 255 included (NBB:531); this
//     reader accepts it too and states it with FlagLengthPrefix255. A long form for a length
//     below 254 is stated with FlagNonCanonicalLength. The data is zero-padded to a multiple of
//     4 counting the prefix; a non-zero padding byte is stated with FlagPaddingNonzero. A
//     length past the end of the blob is refused, as the app refuses it, as ErrTruncated: the
//     framing cannot declare a length above MaxString (spec ruling T7).
//   - A boxed vector is the constructor 0x1cb5c415, an int32 count and the elements (VEC:8,35-37).
//     The bare vector reader (VEC:161-177) reads the same header and refuses another value
//     (VEC:162-166), then the int32 count, checked against the bytes that remain (VEC:167-171),
//     then each element bare (VEC:173-175): int32 (VEC:178-180), int64 (VEC:181-183) or a TL
//     string (VEC:184-186). The app's count check allows 1 byte per element; the walker checks
//     each element's minimum (4 for an int32 or a string, whose prefix and padding take 4 bytes,
//     8 for an int64), which refuses no vector the app can read whole.
//   - A bare id vector (VecL = tgnet/VectorLegacy.java) is a bare int32 or int64 vector whose
//     elements the app wraps as user peers (VecL:27-37, 48-58).
//
// The walker reads three table forms beyond single fields and plain vectors (plan 4I4c W1-W3):
//
//   - Every object is looked up by (expected type, ID) (Key): an ID of another type is an
//     unknown constructor, and the Named of every KObject carries the type it was read as.
//   - A union vector element (a KVector field with Alt) is read by Of when Of holds its ID,
//     whatever the body there, else by Alt; an ID in neither is an unknown constructor naming
//     "Of|Alt".
//   - A bare id vector (KVecInt32 or KVecInt64 with Class) yields KID32 or KID64 elements
//     carrying the class; a bare string vector (KVecString) yields KString elements, each a
//     sub-slice of the input.
//   - A flags:# field gates optional fields by bit in declaration order and flag-only true
//     fields carry no bytes (TLO:15-54); FlagUnknownFlagBits names a set bit that gates nothing
//     in a pinned constructor, which only a table-driven walker can decide.
//
// Decode (walker.go) reads one object by a constructor Table the caller owns (the package
// holds no Telegram data): fields in table order, gated fields by their flags bit, nested
// objects to MaxDepth. It never skips: an object it cannot read (unknown, opaque, a prefix
// body, a bound) ends the whole decode, and every field read before it is returned with a
// Stop saying where (offset and path) and why.
//
// Bounds (limits.go) are package constants a caller cannot widen. Every bound is a typed
// error: an *Error wrapping one sentinel of errors.go, carrying the offset where the failed
// read began. A failed read consumes nothing, and every reading call is one step of the
// MaxSteps work budget, so a read loop over hostile input always terminates.
package tl
