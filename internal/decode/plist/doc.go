// Package plist validates and decodes property lists found in device artifacts.
//
// Only two formats are accepted: Apple binary ("bplist00") and XML. OpenStep and GNUstep text
// plists are refused (ErrUnsupported), as is a plist nested inside another plist's data.
// Every document is checked before the decoding library sees it, so a hostile document costs
// bounded time and memory: the object table, the nesting depth, the expanded node count (a
// shared-reference bomb is counted by a memoized walk, never expanded) and the expanded string
// and data payload are all capped by Limits (DefaultLimits: 1<<20 nodes, depth 64, 64 MiB).
//
// Every error wraps exactly one of ErrMalformed, ErrLimit, ErrUnsupported, ErrInternal or
// ErrNoBudget. ErrInternal is the recovered form of a panic and is a bug to report; it must
// never be reachable from input. The package imports no other Minutiae package: callers
// charge their memory budget through the local Budget interface.
package plist
