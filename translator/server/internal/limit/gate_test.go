package limit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGateAcquireBlocksUntilCapacityIsReleased(t *testing.T) {
	gate := New(1)
	releaseFirst, err := gate.Acquire(t.Context())
	require.NoError(t, err)

	acquired := make(chan func(), 1)
	go func() {
		release, acquireErr := gate.Acquire(t.Context())
		if acquireErr == nil {
			acquired <- release
		}
	}()

	select {
	case <-acquired:
		t.Fatal("second acquisition succeeded while the gate was full")
	default:
	}
	releaseFirst()
	releaseSecond := receive(t, acquired)
	releaseSecond()
}

func TestGateTryAcquireIsNonBlockingAndReleaseIsIdempotent(t *testing.T) {
	gate := New(1)
	release, ok := gate.TryAcquire()
	require.True(t, ok)

	_, ok = gate.TryAcquire()
	require.False(t, ok)

	release()
	release()
	secondRelease, ok := gate.TryAcquire()
	require.True(t, ok)
	secondRelease()
}

func TestGateAcquireReturnsWhenContextIsCancelled(t *testing.T) {
	gate := New(1)
	release, err := gate.Acquire(t.Context())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = gate.Acquire(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestGateAcquireRejectsAlreadyCancelledContextWithoutLeakingCapacity(t *testing.T) {
	gate := New(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	release, err := gate.Acquire(ctx)
	require.Nil(t, release)
	require.ErrorIs(t, err, context.Canceled)

	release, ok := gate.TryAcquire()
	require.True(t, ok)
	release()
}

func receive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-t.Context().Done():
		t.Fatal("test ended before expected channel value arrived")
		var zero T
		return zero
	}
}
