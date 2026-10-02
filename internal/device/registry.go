package device

import (
	"context"
	"fmt"
)

// Registry aggregates the enumerators of all backends.
type Registry struct{ enums []Enumerator }

// NewRegistry returns a registry with the given enumerators.
func NewRegistry(e ...Enumerator) *Registry { return &Registry{enums: e} }

// Register adds an enumerator.
func (r *Registry) Register(e Enumerator) { r.enums = append(r.enums, e) }

// List returns devices of kind (all kinds when empty). A failing backend does
// not hide the others; its error is returned alongside.
func (r *Registry) List(ctx context.Context, kind Kind) ([]Device, []error) {
	var devs []Device
	var errs []error
	for _, e := range r.enums {
		if kind != "" && e.Kind() != kind {
			continue
		}
		ds, err := e.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Kind(), err))
			continue
		}
		devs = append(devs, ds...)
	}
	return devs, errs
}

// Find returns the device with id, or ErrNotFound.
func (r *Registry) Find(ctx context.Context, kind Kind, id string) (Device, error) {
	devs, errs := r.List(ctx, kind)
	for _, d := range devs {
		if d.ID() == id {
			return d, nil
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s (%v)", ErrNotFound, id, errs)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}
