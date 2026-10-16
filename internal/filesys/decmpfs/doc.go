// Package decmpfs decodes Apple's transparent file compression ("decmpfs"), as
// shared by the HFS+ and APFS readers (which may not import each other).
//
// Current state. Only the header parser and the typed errors exist; the
// decoders, the ratio cap and the block-table checks described below are NOT
// implemented yet (Tasks 8 and later).
//
// What will be decoded. Only the two layouts the HFS+ reader already shipped:
// type 3 (a zlib stream, or a stored payload marked by a first byte whose low
// four bits are 0x0F, in the com.apple.decmpfs attribute) and type 4 (zlib
// blocks of 64 KiB in the resource fork, behind a 0x100-byte header and a
// little-endian block table). The layouts come from the format
// description; they will be exercised by builder images only and are NOT validated
// against any file written by an Apple system (none is available). Every other
// type is reported as an *UnsupportedError carrying the type, never guessed:
// the compressed bytes stay reachable through the filesystem's XattrReader.
//
// A decoder (once it exists) never allocates from a declared size: every size
// is bounded by the bytes actually present (a compression ratio cap, the block table against the
// fork, a block count equal to the size) and a table or block that lies is a
// *filesys.CorruptError.
package decmpfs
