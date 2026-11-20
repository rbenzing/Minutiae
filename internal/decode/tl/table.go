package tl

// Kind is how the walker reads one field (spec section 7.2, plan 4I4-P13).
type Kind uint8

// Field kinds. A gated field (Field.Flags set) is read only when its bit is set.
const (
	// KInt32 is a little-endian int32 (Named.Int).
	KInt32 Kind = iota + 1
	// KInt64 is a little-endian int64 (Named.Int).
	KInt64
	// KDouble is an IEEE-754 double (Named.Float).
	KDouble
	// KBool is a boxed bool (Named.Bool).
	KBool
	// KBytes is a TL bytes value (Named.Bytes, a sub-slice of the blob, and Named.Len).
	KBytes
	// KString is a TL string, framed as bytes (Named.Bytes and Named.Len; not validated).
	KString
	// KSecret is a bytes value whose content never enters the result: Named.Len only (spec
	// section 9).
	KSecret
	// KFlags is a flags word, named "flags" or "flags2", that gates the later fields of its
	// object (Named.Int, the unsigned word).
	KFlags
	// KTrue is a flag-only true: no bytes; Named.Bool true when its bit is set, absent else.
	KTrue
	// KObject is a boxed object of the type Field.Of (Named.ID; its fields follow).
	KObject
	// KVector is a boxed vector of boxed objects of the type Field.Of (Named.Len; elements
	// follow at name[i]).
	KVector
	// KVecInt32 is a boxed vector header and bare int32 elements (Named.Len; name[i]).
	KVecInt32
	// KVecInt64 is a boxed vector header and bare int64 elements (Named.Len; name[i]).
	KVecInt64
	// KID32 is an int32 identifier of the class Field.Class (Named.Int, Named.Class; T5).
	KID32
	// KID64 is an int64 identifier of the class Field.Class (Named.Int, Named.Class; T5).
	KID64
)

// Field is one field of a constructor, in declaration order. Of names the type of a KObject
// or KVector; Flags names the flags word ("flags" or "flags2") that gates the field, "" for
// an ungated one, and Bit its bit (0 to 31); Class is the identity class of a KID32 or KID64.
type Field struct {
	Name  string
	Kind  Kind
	Of    string
	Flags string
	Bit   int
	Class string
}

// Body says how much of a constructor the table pins.
type Body uint8

// Bodies.
const (
	// BodyFull lists every field: the object is read whole.
	BodyFull Body = iota + 1
	// BodyPrefix lists the fields up to the last one needed: the decode ends after them with
	// StopPrefix (the walker never skips the unlisted rest).
	BodyPrefix
	// BodyOpaque lists no field: the decode ends at the object with StopOpaque.
	BodyOpaque
)

// Constructor is one TL constructor: its ID, name, the type it belongs to, how much of its
// body is pinned, the fields in order and the source citation of the facts.
type Constructor struct {
	ID     uint32
	Name   string
	Type   string
	Body   Body
	Fields []Field
	Source string
}

// Table maps a constructor ID to its constructor. It is read-only data: the walker never
// changes it, and a table lives in the parser that owns the facts (spec section 8.2).
type Table map[uint32]Constructor
