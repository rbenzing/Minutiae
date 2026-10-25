package ext4

import "github.com/rbenzing/minutiae/internal/filesys"

// AllocatedRuns reports the whole blocks the file's data runs occupy, one run per data run with its length
// rounded up to the block size (filesys.AllocatedRunner). It is derived from the same runs as Runs, so a map cut
// by the end of the image lists only the blocks that could be followed (a prefix), holes and uninitialized
// (preallocated) extents are not data runs and report nothing, and an inline file or a fast symlink (unit 0) has
// no blocks. The slice is fresh on every call.
func (fl *file) AllocatedRuns() []filesys.Run { return filesys.UnitRuns(fl.runs, fl.unit) }

// ReportsSlack declares filesys.SlackReporter: the filesystem occupies exactly [0, Info().Size) of its
// partition and its regular files implement filesys.AllocatedRunner.
func (f *FS) ReportsSlack() bool { return true }
