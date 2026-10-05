package sqlitefile

// FrameState classifies one WAL slot (Format reference, "WAL").
type FrameState uint8

// The states of a WAL slot.
const (
	// FrameCommitted: in the valid chain at or before the last commit frame.
	// The engine applies exactly these.
	FrameCommitted FrameState = iota + 1
	// FrameUncommitted: in the valid chain after the last commit frame.
	FrameUncommitted
	// FrameBroken: the first slot after the chain whose salts match the
	// header but whose page number is 0 or whose checksum fails.
	FrameBroken
	// FrameDetached: a later slot whose salts match the header but which
	// follows a break or a foreign-salt slot.
	FrameDetached
	// FrameStale: its salts differ from the header's: an older generation.
	FrameStale
	// FrameUnanchored: the header is invalid, so no slot ties to a header.
	FrameUnanchored
)

func (s FrameState) String() string {
	switch s {
	case FrameCommitted:
		return "committed"
	case FrameUncommitted:
		return "uncommitted"
	case FrameBroken:
		return "broken"
	case FrameDetached:
		return "detached"
	case FrameStale:
		return "stale"
	case FrameUnanchored:
		return "unanchored"
	}
	return "unknown"
}

// WALFrame is one complete slot of the WAL.
type WALFrame struct {
	Slot           uint32 // 1-based
	Page, DBSize   uint32 // DBSize != 0 marks a commit frame
	Salt1, Salt2   uint32
	Check1, Check2 uint32
	State          FrameState
	Linked         bool  // verifies from the previous slot's stored checksum
	Generation     int   // index into WALInfo.Generations
	Offset         int64 // of the frame header; the page image is at Offset+24
}

// WALGeneration is a run of consecutive slots with equal salts, each linked to
// the one before it.
type WALGeneration struct {
	Salt1, Salt2     uint32
	FirstSlot, Slots uint32
	// Anchored is set for the generation that holds the valid chain (the
	// current one); it is false for every generation of a WAL whose header
	// is invalid, and when slot 1 does not verify.
	Anchored bool
	// Age is (header salt-1 - Salt1) mod 2^32: 0 for the current generation,
	// larger for older ones, whatever their slots. Meaningful only with a
	// valid header (else 0).
	Age uint32
	// Commits counts the slots of the generation that carry a database size
	// (commit frames), whatever their state.
	Commits uint32
}

// WALInfo is what the scan found out about the WAL as a whole.
type WALInfo struct {
	Present, HeaderValid bool
	HeaderProblem        string
	Size                 int64
	BigEndianChecksums   bool
	// PageSize is the page size the slots were cut by: the header's when it
	// is a power of two in 512..65536, else the database's, else 0 (no slots).
	PageSize, CheckpointSeq uint32
	Salt1, Salt2            uint32
	FrameSlots              uint32 // complete slots in the file (saturates at MaxUint32)
	TrailingBytes           int64  // bytes after the last complete slot (a torn frame)
	// FramesValid is the length of the anchored valid chain, FramesCommitted
	// the frames up to LastCommit (the ones the engine applies).
	FramesValid, FramesCommitted uint32
	LastCommit                   uint32 // slot of the last valid commit frame, 0 = none
	Commits                      uint32
	FramesUncommitted            uint32
	FramesBroken                 uint32
	FramesDetached               uint32
	FramesStale                  uint32
	// DBPagesAfterCommit is the database size the last commit frame claims:
	// an upper bound only, never a size to allocate from.
	DBPagesAfterCommit uint32
	// MaxPageNumber is the highest page number among the committed frames.
	MaxPageNumber uint32
	Generations   []WALGeneration
	UsedByLive    bool // set by AttachWAL
}

// WALScan is the result of ScanWAL. Frames lists every complete slot scanned,
// in slot order.
type WALScan struct {
	Info     WALInfo
	Frames   []WALFrame
	Warnings []Warning

	// latest maps a page number to the slot of its newest committed frame.
	latest map[uint32]uint32
}
