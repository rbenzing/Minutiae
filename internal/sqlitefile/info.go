package sqlitefile

import "fmt"

// Encoding is the text encoding of a database (header offset 56).
type Encoding uint8

// The text encodings.
const (
	EncUTF8    Encoding = 1
	EncUTF16LE Encoding = 2
	EncUTF16BE Encoding = 3
)

func (e Encoding) String() string {
	switch e {
	case EncUTF8:
		return "UTF-8"
	case EncUTF16LE:
		return "UTF-16le"
	case EncUTF16BE:
		return "UTF-16be"
	}
	return fmt.Sprintf("encoding(%d)", uint8(e))
}

// AutoVacuum is the auto-vacuum mode the header records.
type AutoVacuum uint8

// The auto-vacuum modes.
const (
	AVNone AutoVacuum = iota
	AVFull
	AVIncremental
)

// Info is what the database header says, plus the geometry derived from it
// and the file size. Nothing in it is read from pages other than page 1.
type Info struct {
	FileSize           int64
	PageSize, Reserved int
	UsableSize         int
	Encoding           Encoding // UTF-8 when the stored value is not 1..3
	EncodingValid      bool     // the stored value is 1, 2 or 3
	HeaderPages        uint32   // field at offset 28 as stored
	HeaderPagesValid   bool     // non-zero and file change counter == version-valid-for
	FilePages          uint32   // whole pages in the file
	PageCount          uint32   // pages the engine would use (declared or file-derived; an upper bound, not an allocation size)
	ChangeCounter      uint32
	VersionValidFor    uint32
	SQLiteVersion      uint32
	SchemaCookie       uint32
	SchemaFormat       uint32
	FreelistTrunk      uint32 // header value
	FreelistCount      uint32 // header value
	AutoVacuum         AutoVacuum
	LargestRoot        uint32
	UserVersion        int32
	ApplicationID      uint32
	WriteVersion       uint8
	ReadVersion        uint8
	LockBytePage       uint32   // 0 when the file is too small to contain it
	EngineRefuses      []string // why the SQLite engine would refuse this file; it is opened regardless
}
