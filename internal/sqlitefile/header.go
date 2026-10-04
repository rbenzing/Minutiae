package sqlitefile

import (
	"encoding/binary"
	"fmt"
	"math"
)

// parseHeader decodes the 100-byte header in buf for a file of size bytes.
// A header that makes page 1 unreadable is an error (*CorruptError); a header
// the engine would refuse but that is still readable is opened, with a
// warning and a reason in Info.EngineRefuses.
func parseHeader(buf []byte, size int64, w *warnings) (Info, error) {
	be := binary.BigEndian
	var i Info
	i.FileSize = size

	field := be.Uint16(buf[16:])
	i.PageSize = headerPageSize(field)
	if i.PageSize == 0 {
		return Info{}, &CorruptError{
			File: FileDB, Page: 1,
			Reason: fmt.Sprintf("page size field %#04x is neither a power of two in 512..32768 nor 1 (65536)", field),
		}
	}
	i.Reserved = int(buf[20])
	i.UsableSize = i.PageSize - i.Reserved
	if i.UsableSize < 480 {
		return Info{}, &CorruptError{
			File: FileDB, Page: 1,
			Reason: fmt.Sprintf("usable size %d (page size %d, %d reserved bytes) is below 480", i.UsableSize, i.PageSize, i.Reserved),
		}
	}
	if size < int64(i.PageSize) {
		return Info{}, &CorruptError{
			File: FileDB, Page: 1,
			Reason: fmt.Sprintf("page 1 (page size %d) is not wholly inside the file (%d bytes)", i.PageSize, size),
		}
	}

	i.WriteVersion, i.ReadVersion = buf[18], buf[19]
	i.ChangeCounter = be.Uint32(buf[24:])
	i.HeaderPages = be.Uint32(buf[28:])
	i.FreelistTrunk = be.Uint32(buf[32:])
	i.FreelistCount = be.Uint32(buf[36:])
	i.SchemaCookie = be.Uint32(buf[40:])
	i.SchemaFormat = be.Uint32(buf[44:])
	i.LargestRoot = be.Uint32(buf[52:])
	i.UserVersion = int32(be.Uint32(buf[60:]))
	i.ApplicationID = be.Uint32(buf[68:])
	i.VersionValidFor = be.Uint32(buf[92:])
	i.SQLiteVersion = be.Uint32(buf[96:])
	if i.LargestRoot != 0 {
		i.AutoVacuum = AVFull
		if be.Uint32(buf[64:]) != 0 {
			i.AutoVacuum = AVIncremental
		}
	}

	add := func(code string, off int64, msg string) {
		w.add(Warning{Code: code, File: FileDB, Page: 1, Offset: off, Msg: msg})
	}
	refuse := func(reason string) { i.EngineRefuses = append(i.EngineRefuses, reason) }

	// Text encoding: 1..3; 0 is "unset" and read as UTF-8; anything else is
	// read as UTF-8 too, and said.
	i.Encoding = EncUTF8
	switch enc := be.Uint32(buf[56:]); {
	case enc >= 1 && enc <= 3:
		i.Encoding, i.EncodingValid = Encoding(enc), true
	case enc != 0:
		add(WarnHdrEncodingInvalid, 56, fmt.Sprintf("text encoding field is %d, not 1..3; read as UTF-8", enc))
		if engineRefusesBadEncoding {
			refuse(fmt.Sprintf("text encoding field is %d", enc))
		}
	}

	// Payload fractions are constants of the format; the reader uses them
	// whatever the header says.
	if buf[21] != 64 || buf[22] != 32 || buf[23] != 32 {
		add(WarnHdrFractions, 21, fmt.Sprintf("payload fractions are %d/%d/%d, not 64/32/32", buf[21], buf[22], buf[23]))
		refuse(fmt.Sprintf("payload fractions are %d/%d/%d, not 64/32/32", buf[21], buf[22], buf[23]))
	}
	if i.WriteVersion > 2 || i.ReadVersion > 2 {
		add(WarnHdrVersionBytes, 18, fmt.Sprintf("write version %d, read version %d (1 and 2 are defined)", i.WriteVersion, i.ReadVersion))
	}
	if i.ReadVersion > 2 {
		refuse(fmt.Sprintf("read version %d is above 2", i.ReadVersion))
	}

	// Page count: the header's, when the file change counter vouches for it,
	// else the file's.
	whole := size / int64(i.PageSize)
	if whole > math.MaxUint32 {
		whole = math.MaxUint32
		add(WarnPageCountClamped, 0, "the file holds more whole pages than a page number can name; counted as 4294967295")
	}
	i.FilePages = uint32(whole)
	if rest := size % int64(i.PageSize); rest != 0 && whole < math.MaxUint32 {
		add(WarnTruncatedFile, 0, fmt.Sprintf("trailing partial page of %d bytes ignored", rest))
	}
	i.HeaderPagesValid = i.HeaderPages != 0 && i.ChangeCounter == i.VersionValidFor
	switch {
	case i.HeaderPagesValid:
		i.PageCount = i.HeaderPages
		if i.HeaderPages > i.FilePages {
			add(WarnTruncatedFile, 28, fmt.Sprintf("header declares %d pages, the file holds %d", i.HeaderPages, i.FilePages))
			refuse(fmt.Sprintf("header declares %d pages, the file holds %d", i.HeaderPages, i.FilePages))
		}
	default:
		i.PageCount = i.FilePages
		if i.HeaderPages != 0 {
			add(WarnHdrCounterMismatch, 24, fmt.Sprintf("file change counter %d differs from version-valid-for %d: the header page count is not trusted", i.ChangeCounter, i.VersionValidFor))
		}
	}

	if size > lockByteOffset {
		i.LockBytePage = LockBytePage(i.PageSize)
	}
	return i, nil
}
