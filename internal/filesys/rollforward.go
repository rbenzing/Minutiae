package filesys

// RollForwarder is implemented by a filesystem whose log may hold nodes written after its last
// checkpoint (F2FS roll-forward). It reports what a replay would find; it never replays and never
// changes the live view. Callers find it with As.
type RollForwarder interface {
	// RollForward calls visit for every chain node in order (fresh slices each call); visit returning
	// false stops early. The chain summary describes the whole chain either way.
	RollForward(visit func(RollForwardNode) bool) (RollForwardChain, error)
	// PostCheckpointFiles lists the files that exist only in the chain, with their maps (never bytes).
	PostCheckpointFiles() ([]PostCheckpointFile, error)
}

// RollForwardChain summarises the chain of nodes written after the last checkpoint.
type RollForwardChain struct {
	CheckpointVersion uint64
	CheckpointPack    int
	FooterVersion     uint64 // the footer cp_ver a chain node must carry
	CRCRecovery       bool
	NoCRCRecovery     bool
	Unmount           bool
	Start             uint64 // filesystem block number of the first block examined
	Nodes             int
	Stop              string // "<reason>@<block>": version, outside-main, cycle, footer, image-end, cap
	Complete          bool   // false when the stop leaves later nodes unknown
	Files             []RollForwardFile
	Warnings          []string
}

// RollForwardFile is one file the chain touches.
type RollForwardFile struct {
	Ino, Pino      uint32
	Kind           string // post-checkpoint | live-update | not-replayed
	ID             string // "rollforward:<ino>" for post-checkpoint files, else ""
	Name           string // display form; "" without an inode node
	HasInode       bool
	InodeBlock     uint64 // 0 when absent
	LastFsyncBlock uint64 // 0 when absent
	Size           int64  // meaningful only with HasInode
}

// RollForwardNode is one node block of the chain. Block numbers are filesystem-relative.
type RollForwardNode struct {
	Index               int
	Block, Next         uint64
	NID, Ino, Ofs, Flag uint32
	CPVer               uint64
	Kind                string // inode | direct | indirect | double-indirect | xattr
	Fsync, Dentry, Cold bool
	LiveIno             bool
	WouldReplay         bool
	Inode               *RollForwardInode // inode nodes only
	Addrs               []uint32          // data address slots of an inode or direct node, nil otherwise
	NIDs                []uint32          // i_nid of an inode, the child ids of an indirect node, nil otherwise
}

// RollForwardInode is the inode a chain inode node carries.
type RollForwardInode struct {
	Mode, UID, GID, Links, Pino, Flags uint32
	Size                               int64
	Times                              Times
	Name                               string // display form
	RawName                            []byte
	Inline                             uint8
	Encrypted, InlineData, Compressed  bool
	Checksum                           string // ok | bad | not-recorded
	Err                                string // the parse failure, "" when it parsed
}

// PostCheckpointFile is a file that exists only in the chain, with its recovery map (never bytes).
type PostCheckpointFile struct {
	ID         string // "rollforward:<ino>"
	Ino, Pino  uint32
	Name       string // display form
	Type       EntryType
	Size       int64
	Candidates []Candidate // at most one in 3H
	Err        error       // typed, as Recoverable's errors
}
