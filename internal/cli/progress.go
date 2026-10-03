package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
)

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// newProgress prints a throttled single-line progress indicator to w.
func newProgress(w io.Writer, label string) device.ProgressFunc {
	var last time.Time
	return func(done, total int64) {
		finished := total > 0 && done >= total
		if !finished && time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		if total > 0 {
			fmt.Fprintf(w, "\r%s: %s / %s (%.1f%%)   ", label, humanBytes(done), humanBytes(total), float64(done)*100/float64(total))
		} else {
			fmt.Fprintf(w, "\r%s: %s   ", label, humanBytes(done))
		}
	}
}
