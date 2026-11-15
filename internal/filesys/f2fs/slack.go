package f2fs

import "github.com/rbenzing/minutiae/internal/filesys"

// AllocatedRuns reports the whole blocks the file's data runs occupy, one run per data run with its length
// rounded up to the block size (filesys.AllocatedRunner). It is derived from the same runs as Runs, so a node
// chain damaged part way lists only the blocks that could be followed (a prefix), holes (NULL_ADDR) and blocks
// mapped to NEW_ADDR are not data runs and report nothing, and an inline file has no blocks. The slice is fresh
// on every call.
func (fl *file) AllocatedRuns() []filesys.Run {
	if fl.inline {
		return nil
	}
	return filesys.UnitRuns(fl.runs, blockSize)
}

// ReportsSlack declares filesys.SlackReporter: the filesystem occupies exactly [0, Info().Size) of its
// partition and its regular files implement filesys.AllocatedRunner.
func (f *FS) ReportsSlack() bool { return true }
