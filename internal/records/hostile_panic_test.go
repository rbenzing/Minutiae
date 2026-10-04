package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

type panicsWhenPrinted struct{}

func (panicsWhenPrinted) Error() string  { panic("Error() panicked") }
func (panicsWhenPrinted) String() string { panic("String() panicked") }

// TestValidatorPanicWithHostileValueIsTyped: formatting the panic value cannot
// itself panic (a panic value whose Error or String method panics).
func TestValidatorPanicWithHostileValueIsTyped(t *testing.T) {
	for name, v := range map[string]any{
		"hostile error":   panicsWhenPrinted{},
		"nil pointer err": (*panicsWhenPrinted)(nil),
		"struct":          struct{ A, B int }{1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			ty := "hostile_" + strings.ReplaceAll(name, " ", "_")
			registerTemp(t, records.Type{Name: ty, PayloadVersion: 1, Validate: func(map[string]any) error { panic(v) }})
			r := base()
			r.Type = ty
			_, err := records.Prepare(r, testArt)
			if !errors.Is(err, records.ErrInvalidPayload) || !strings.Contains(err.Error(), ty) {
				t.Fatalf("err = %v, want ErrInvalidPayload naming the type", err)
			}
		})
	}
}
