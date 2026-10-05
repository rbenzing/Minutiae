package parse

import (
	"context"
	"errors"
	"testing"
)

func TestTickPollsEveryTickEvery(t *testing.T) {
	if TickEvery != 4096 {
		t.Fatalf("TickEvery = %d, want 4096", TickEvery)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 3*TickEvery + 1 {
		err := Tick(ctx, i)
		if i%TickEvery == 0 {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Tick(cancelled, %d) = %v, want context.Canceled", i, err)
			}
		} else if err != nil {
			t.Fatalf("Tick(cancelled, %d) = %v, want nil: the context must not be polled off a multiple of TickEvery", i, err)
		}
	}
	live := context.Background()
	for _, i := range []int{0, 1, TickEvery, -1, -TickEvery, -TickEvery - 1} {
		if err := Tick(live, i); err != nil {
			t.Fatalf("Tick(live, %d) = %v", i, err)
		}
	}
	for _, i := range []int{-1, -2, -TickEvery + 1, -TickEvery - 1, int(^uint(0) >> 1), -int(^uint(0)>>1) - 1} {
		if err := Tick(ctx, i); i%TickEvery != 0 && err != nil {
			t.Fatalf("Tick(cancelled, %d) = %v, want nil", i, err)
		}
	}
	if err := Tick(ctx, -TickEvery); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick(cancelled, -TickEvery) = %v", err)
	}
	var noCtx context.Context // a nil context must not panic
	if err := Tick(noCtx, 0); err != nil {
		t.Fatalf("Tick(nil, 0) = %v", err)
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{ErrBudget, ErrSealed, ErrRecordCap, ErrRejectedCap, ErrProbeLimit, ErrPayloadVersionMismatch, ErrNoPayloadContract, ErrUndeclaredType, ErrForeignPlatform, ErrInputChanged, ErrWarningCap, ErrRefusedRecords}
	for i, a := range all {
		if a == nil || a.Error() == "" {
			t.Fatalf("sentinel %d is nil or empty", i)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("sentinels %d and %d are the same error", i, j)
			}
		}
	}
}

func TestApplicabilityStatuses(t *testing.T) {
	got := map[string]string{
		"Applicable": Applicable, "NotApplicable": NotApplicable,
		"UnsupportedSchema": UnsupportedSchema, "Encrypted": Encrypted,
	}
	want := map[string]string{
		"Applicable": "applicable", "NotApplicable": "not-applicable",
		"UnsupportedSchema": "unsupported-schema", "Encrypted": "encrypted",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}
