package examine

import "github.com/rbenzing/minutiae/internal/filesys"

// ReportsSlack forwards filesys.ReportsSlack to the wrapped filesystem (safeFS has no Underlying, so the
// capability needs its own method). A panic of the reader is false: a filesystem that cannot say whether it
// reports its allocation is refused, never assumed.
func (s *safeFS) ReportsSlack() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return filesys.ReportsSlack(s.fs)
}
