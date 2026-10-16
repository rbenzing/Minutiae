package ewf

import "math"

// maxDeflateRatio is the best compression ratio deflate can reach (1032:1).
const maxDeflateRatio = 1032

// SizeClaim compares the media size the volume section claims with the bytes of
// the segment files that would have to hold it.
type SizeClaim struct {
	Media       int64 // media size claimed by the volume section
	Stored      int64 // sum of the segment file sizes
	Implausible bool  // Media > Stored * 1032: no compression could fit it
}

// SizeClaim returns the claim and whether it is implausible.
func (r *Reader) SizeClaim() SizeClaim {
	var stored int64
	for _, s := range r.segs {
		if s.Size > math.MaxInt64-stored {
			stored = math.MaxInt64
			break
		}
		stored += s.Size
	}
	c := SizeClaim{Media: r.geo.size, Stored: stored}
	c.Implausible = c.Media > c.limit()
	return c
}

// limit is the most media Stored bytes can hold at deflate's best ratio,
// saturating at MaxInt64.
func (c SizeClaim) limit() int64 {
	if c.Stored > math.MaxInt64/maxDeflateRatio {
		return math.MaxInt64
	}
	return c.Stored * maxDeflateRatio
}

// warnSizeClaim adds the warning for an implausible claim. A covered chunk is
// stored in at least chunkSize/1032 bytes, so an honest image cannot reach it;
// the claim is a claim, not data.
func (o *opener) warnSizeClaim() {
	if c := o.r.SizeClaim(); c.Implausible {
		o.r.warn.add("implausible size: the volume section claims %d bytes of media, but the %d bytes of segment files cannot hold more than %d bytes even at the best deflate ratio (%d:1); the size is a claim, not data", c.Media, c.Stored, c.limit(), maxDeflateRatio)
	}
}
