package ios

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
)

var (
	errEndUnconfirmed  = errors.New("device ended the transfer with an error block after the file data and no recorded size confirms the file is complete")
	errEndSizeMismatch = errors.New("device ended the transfer with an error block after the file data and the backup's own record gives another size")
)

// sizeOracle returns the size the backup's own metadata declares for the
// staged file at rel (slash-separated, relative to staging). known is false
// when the metadata does not say; err explains why it could not be consulted.
type sizeOracle func(rel string) (size int64, known bool, err error)

// maxEndingSamples bounds the per-file lines kept in acquire.stats; the counts
// stay exact.
const maxEndingSamples = 100

// endingReport counts how the files that ended with a device error block
// after data were judged.
type endingReport struct {
	Confirmed, Mismatch, Unconfirmed int
	Samples                          []string
}

// judgeEndings decides, per file in ends, whether its received bytes are
// proven complete. cut holds a cause for every file that is not: the artifact
// is promoted flagged incomplete with that cause. A nil oracle proves nothing.
func judgeEndings(ends map[string]mb2.RemoteEnd, oracle sizeOracle) (cut map[string]error, rep endingReport) {
	cut = map[string]error{}
	for _, rel := range slices.Sorted(maps.Keys(ends)) {
		end := ends[rel]
		var (
			size  int64
			known bool
			oerr  error
		)
		if oracle != nil {
			size, known, oerr = oracle(rel)
		}
		switch {
		case oerr == nil && known && size == end.Bytes:
			rep.Confirmed++
			continue
		case oerr == nil && known:
			rep.Mismatch++
			cut[rel] = fmt.Errorf("%w: received %d bytes, recorded %d", errEndSizeMismatch, end.Bytes, size)
		default:
			rep.Unconfirmed++
			cut[rel] = errors.Join(errEndUnconfirmed, oerr)
		}
		if len(rep.Samples) < maxEndingSamples {
			name := end.Path // the path as the device sent it; the key may be case-folded
			if name == "" {
				name = rel
			}
			// The path and message are raw device bytes: always quoted for display.
			rep.Samples = append(rep.Samples, fmt.Sprintf("%q: code 0x%02x, %d bytes, %q", name, end.Code, end.Bytes, end.Message))
		}
	}
	return cut, rep
}

// backupSizes returns the oracle for a finished staging directory: none until
// the backup's Manifest.db is consulted (Task 4).
func backupSizes(_, _ string, _ mb2.Result) sizeOracle { return nil }
