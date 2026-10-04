package apfs

// Extended fields: xf_blob_t {num_exts u16, used_data u16}, then num_exts
// x_field_t {type u8, flags u8, size u16}, then the values in the same order,
// each padded to 8 bytes.
const (
	xfInodeName    = 4 // INO_EXT_TYPE_NAME
	xfInodeDstream = 8 // INO_EXT_TYPE_DSTREAM

	dstreamMinSize = 8 // j_dstream_t starts with the u64 size
	dstreamCrypto  = 16
)

// xfield is one decoded extended field; data aliases the blob.
type xfield struct {
	typ, flags uint8
	data       []byte
}

// parseXfields decodes an xf_blob_t. Problems are reported through warn (which
// may be nil) and never stop the caller: a blob whose descriptors do not fit is
// ignored whole, a field that runs past the blob ends the list, and a
// duplicate type is ignored in favour of the first.
func parseXfields(b []byte, warn func(format string, a ...any)) []xfield {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	if len(b) == 0 {
		return nil
	}
	if len(b) < 4 {
		warn("extended fields: %d bytes are shorter than the 4-byte header; ignored", len(b))
		return nil
	}
	num := int(le.Uint16(b))
	used := int(le.Uint16(b[2:]))
	body := b[4:]
	if num*4 > len(body) {
		warn("extended fields: %d descriptors do not fit %d bytes; ignored", num, len(body))
		return nil
	}
	descs, data := body[:num*4], body[num*4:]
	if used > len(data) {
		warn("extended fields: used_data %d exceeds the %d bytes present", used, len(data))
	} else {
		data = data[:used]
	}
	if used%8 != 0 {
		warn("extended fields: used_data %d is not a multiple of 8", used)
	}
	var (
		out  []xfield
		seen [256]bool
		off  int
	)
	for i := range num {
		t, fl, sz := descs[4*i], descs[4*i+1], int(le.Uint16(descs[4*i+2:]))
		if sz > len(data)-off {
			warn("extended field %d (type %d, %d bytes at offset %d) runs past the %d bytes of data; the rest is ignored", i, t, sz, off, len(data))
			break
		}
		if seen[t] {
			warn("extended field type %d appears more than once; the later copy is ignored", t)
		} else {
			seen[t] = true
			out = append(out, xfield{typ: t, flags: fl, data: data[off : off+sz : off+sz]})
		}
		off = (off + sz + 7) &^ 7
	}
	return out
}
