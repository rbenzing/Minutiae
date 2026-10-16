package ios

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/rbenzing/minutiae/internal/ios/manifestdb"
	"github.com/rbenzing/minutiae/internal/ios/mb2"
)

var (
	errEndUnconfirmed  = errors.New("device ended the transfer with an error block after the file data and the backup's Manifest.db gives no size consistent with the bytes received")
	errEndSizeMismatch = errors.New("device ended the transfer with an error block after the file data and the backup's Manifest.db gives another size")
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

var (
	// readManifest and foldKey are variables so a test can count or replace them.
	readManifest = manifestdb.Read
	foldKey      = mb2.FoldKey
)

// fileIDOf maps a staged path <udid>/<id[0:2]>/<id> to its file ID. rel is the
// key judgeEndings receives: the folded key where the host file system folds
// case, otherwise the path exactly as the device sent it (the two are the same
// string then). The udid prefix is compared in that same folded or exact form.
func fileIDOf(rel, udid string) (string, bool) {
	dir, id := path.Split(rel)
	if !manifestdb.ValidFileID(id) || dir != foldKey(udid)+"/"+id[:2]+"/" {
		return "", false
	}
	return id, true
}

// backupSizes returns the oracle for a finished staging directory: the sizes
// <udid>/Manifest.db declares for the files staged as <udid>/<id[0:2]>/<id>.
// It is built only when a file ended with an error block, and it keeps only
// those files' IDs. When the manifest cannot be used (absent, encrypted,
// damaged, too large, itself cut off or not proven complete) every lookup
// carries that reason and the file stays unconfirmed.
func backupSizes(ctx context.Context, staging, udid string, res mb2.Result) sizeOracle {
	manifestRel := udid + "/Manifest.db"
	unavailable := func(err error) sizeOracle {
		return func(string) (int64, bool, error) { return 0, false, fmt.Errorf("the manifest is not usable: %w", err) }
	}
	manifestKey := foldKey(manifestRel)
	for _, rel := range res.Incomplete {
		if foldKey(rel) == manifestKey {
			return unavailable(errors.New("its transfer was cut off"))
		}
	}
	if _, ended := res.RemoteEnded[manifestKey]; ended {
		return unavailable(errors.New("it ended with a device error block after data"))
	}
	p := filepath.Join(staging, udid, "Manifest.db")
	fi, err := os.Lstat(p)
	if err != nil {
		return unavailable(err)
	}
	if !fi.Mode().IsRegular() {
		return unavailable(fmt.Errorf("not a regular file (%s)", fi.Mode()))
	}
	f, err := os.Open(p)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = f.Close() }()
	wanted := map[string]bool{}
	for rel := range res.RemoteEnded {
		if id, ok := fileIDOf(rel, udid); ok {
			wanted[id] = true
		}
	}
	declared, err := readManifest(ctx, f, fi.Size(), func(id string) bool { return wanted[id] })
	if err != nil {
		return unavailable(err)
	}
	// declared holds only the wanted IDs, in memory, so the file can be closed.
	return func(rel string) (int64, bool, error) {
		id, ok := fileIDOf(rel, udid)
		if !ok {
			return 0, false, nil
		}
		n, known := declared.Size(id)
		return n, known, nil
	}
}

// confirmedBasis is what "confirmed" means in the audited stats: the size is
// consistent with the Manifest.db the same device wrote, which is not an
// independent check of the file.
const confirmedBasis = "consistent with the backup's Manifest.db"
