package filesys

import "context"

// RecoverPreparer is implemented by a filesystem whose Recoverer needs one pass over the image before
// it can answer (for example a free-space scan). Recovery planning calls it once, after the deleted
// entries are enumerated and only when there is at least one. It must honour ctx, be idempotent, and
// leave nothing half-built when cancelled.
type RecoverPreparer interface {
	PrepareRecovery(ctx context.Context) error
}

// PrepareRecovery runs the RecoverPreparer of fsys (found with As); a filesystem without one needs no
// preparation and the answer is nil.
func PrepareRecovery(ctx context.Context, fsys FileSystem) error {
	p, ok := As[RecoverPreparer](fsys)
	if !ok {
		return nil
	}
	return p.PrepareRecovery(ctx)
}
