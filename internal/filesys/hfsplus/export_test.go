package hfsplus

// Test-only accessors, so the external hfsplus_test package can check
// internals without widening the public API.

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// Base returns the byte offset of the volume from the start of the ReaderAt.
func (f *FS) Base() int64 { return f.base }

// ReadVolume reads from the metadata view of the volume, whose offset 0 is
// the start of the volume (Base() bytes into the image).
func (f *FS) ReadVolume(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }

// JournalState names the journal state Open found: "none", "clean",
// "pending" or "unknown".
func (f *FS) JournalState() string {
	switch f.journal {
	case journalClean:
		return "clean"
	case journalPending:
		return "pending"
	case journalUnknown:
		return "unknown"
	}
	return "none"
}
