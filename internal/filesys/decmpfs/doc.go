// Package decmpfs decodes Apple's transparent file compression ("decmpfs"), as
// shared by the HFS+ and APFS readers (which may not import each other).
//
// Current state. The header parser, the typed errors and the zlib decoders
// for types 3 and 4 exist (Open, with the ratio cap, the output ceiling and
// the block-table checks described below). No filesystem package imports
// decmpfs yet: hfsplus still carries its own copy of the decoder and apfs
// reports compressed files as unsupported (Task 9 and later switch them over).
//
// What is decoded. Only the two layouts the HFS+ reader already shipped:
// type 3 (a zlib stream, or a stored payload marked by a first byte whose low
// four bits are 0x0F, in the com.apple.decmpfs attribute) and type 4 (zlib
// blocks of 64 KiB in the resource fork, behind a 0x100-byte header and a
// little-endian block table). The layouts come from the format description;
// they are exercised by builder images only and are NOT validated against
// any file written by an Apple system (none is available). Every other type is
// reported as an *UnsupportedError carrying the type, never guessed: the
// compressed bytes stay reachable through the filesystem's XattrReader.
//
// The decoder never allocates from a declared size. A block or inline payload
// is inflated through a read capped at the declared size plus one byte into a
// buffer that grows with the bytes produced. A declared size above 16 MiB, or
// above 1100 times the compressed length, is refused before anything is
// inflated; so is an inline payload over MaxInlinePayload, a block table that
// does not match the size, and a table entry outside the fork. Each of these
// is a *filesys.CorruptError.
package decmpfs
