// Package decmpfs decodes Apple's transparent file compression ("decmpfs"), as
// shared by the HFS+ and APFS readers (which may not import each other).
//
// What is decoded. Only the two layouts the HFS+ reader already shipped:
// type 3 (a zlib stream, or a stored payload marked by a first byte whose low
// four bits are 0x0F, in the com.apple.decmpfs attribute) and type 4 (zlib
// blocks of 64 KiB in the resource fork, behind a 0x100-byte header and a
// little-endian block table). The layouts are implemented from the format
// description; they are exercised by builder images only and are NOT validated
// against any file written by an Apple system (none is available). Every other
// type is reported as an *UnsupportedError carrying the type, never guessed:
// the compressed bytes stay reachable through the filesystem's XattrReader.
//
// A decoder never allocates from a declared size: every size is bounded by the
// bytes actually present (a compression ratio cap, the block table against the
// fork, a block count equal to the size) and a table or block that lies is a
// *filesys.CorruptError.
package decmpfs
