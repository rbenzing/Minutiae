// Package carve finds whole contiguous objects of known file formats in
// unallocated space, volume gaps, raw partitions and artifacts, without a
// filesystem (plan 3E).
//
// It is a pure parser package: it works on io.ReaderAt, imports no Minutiae
// package and can never write to a case. The signature table is data (Set,
// Type, Pattern), every built-in format has a bounded structural validator that
// computes the exact length of the object it walks, and nothing is ever
// decompressed. A validator reads only through a Window, so a declared size
// (a box, chunk or segment length) moves a cursor inside the window and never
// drives an allocation or an unbounded loop.
package carve
