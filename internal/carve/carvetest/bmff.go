package carvetest

import (
	"encoding/binary"
	"slices"
)

// Box returns an ISO-BMFF box: a big-endian size32, the type and the payload.
func Box(typ string, payload ...[]byte) []byte {
	body := slices.Concat(payload...)
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	b = append(b, typ...)
	return append(b, body...)
}

// FullBox returns a box whose payload starts with a version byte and 24 bits
// of flags.
func FullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return Box(typ, append([][]byte{head}, payload...)...)
}

// LargeBox returns a box with a size field of 1 and a 64-bit largesize.
func LargeBox(typ string, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, 1)
	b = append(b, typ...)
	b = binary.BigEndian.AppendUint64(b, uint64(16+len(payload)))
	return append(b, payload...)
}

// BMFFOpts shapes a synthetic ISO-BMFF file built by an encoder independent
// of the validator. Zero values give an "isom" file: ftyp, moov (mvhd and a
// trak), mdat. With HEIF the file holds a meta box (hdlr "pict", pitm) instead
// of a moov, and the brand defaults to "heic".
type BMFFOpts struct {
	Brand        string   // major brand, 4 bytes; "" = "isom" ("heic" with HEIF)
	Compat       []string // compatible brands
	MdatFirst    bool     // mdat before moov (or meta)
	LargeMdat    bool     // mdat with a 64-bit largesize
	SizeZeroLast bool     // the mdat box carries size 0 (extends to the end)
	HEIF         bool
	NoMoov       bool // no moov (non-HEIF)
	NoMeta       bool // no meta (HEIF)
	BadChildren  bool // 3 stray bytes at the end of the moov (meta) payload
	Trailer      []byte
}

// MP4 builds the file per o.
func MP4(o BMFFOpts) []byte {
	if o.Brand == "" {
		o.Brand = "isom"
		if o.HEIF {
			o.Brand = "heic"
		}
	}
	ftyp := Box("ftyp", []byte(o.Brand), make([]byte, 4))
	for _, c := range o.Compat {
		ftyp = append(ftyp, c...)
	}
	binary.BigEndian.PutUint32(ftyp, uint32(len(ftyp)))
	var stray []byte
	if o.BadChildren {
		stray = []byte{1, 2, 3}
	}
	var main []byte
	switch {
	case o.HEIF && !o.NoMeta:
		hdlr := FullBox("hdlr", 0, 0, make([]byte, 4), []byte("pict"), make([]byte, 12), []byte{0})
		main = FullBox("meta", 0, 0, hdlr, FullBox("pitm", 0, 0, []byte{0, 1}), stray)
	case !o.HEIF && !o.NoMoov:
		trak := Box("trak", FullBox("tkhd", 0, 3, make([]byte, 80)))
		main = Box("moov", FullBox("mvhd", 0, 0, make([]byte, 96)), trak, stray)
	}
	payload := make([]byte, 40)
	for i := range payload {
		payload[i] = byte(i*13 + 5)
	}
	var mdat []byte
	if o.LargeMdat {
		mdat = LargeBox("mdat", payload)
	} else {
		mdat = Box("mdat", payload)
	}
	if o.SizeZeroLast {
		binary.BigEndian.PutUint32(mdat, 0)
	}
	b := ftyp
	if o.MdatFirst {
		b = slices.Concat(b, mdat, main)
	} else {
		b = slices.Concat(b, main, mdat)
	}
	return append(b, o.Trailer...)
}
