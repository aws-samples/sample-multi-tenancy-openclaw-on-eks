package lock_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/lock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockLocker_AcquireAndRelease(t *testing.T) {
	l := lock.NewMock()
	ctx := context.Background()

	acquired, err := l.AcquireWakeLock(ctx, "tenant-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired)

	// Same tenant — should fail (already locked)
	acquired2, err := l.AcquireWakeLock(ctx, "tenant-1", time.Minute)
	require.NoError(t, err)
	assert.False(t, acquired2)

	// Different tenant — should succeed
	acquired3, err := l.AcquireWakeLock(ctx, "tenant-2", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired3)

	// Release tenant-1, then re-acquire
	err = l.ReleaseWakeLock(ctx, "tenant-1")
	require.NoError(t, err)

	acquired4, err := l.AcquireWakeLock(ctx, "tenant-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired4)
}

func TestMockLocker_ConcurrentAcquire(t *testing.T) {
	l := lock.NewMock()
	ctx := context.Background()
	tenantID := "contested-tenant"

	results := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			ok, _ := l.AcquireWakeLock(ctx, tenantID, time.Minute)
			results <- ok
		}()
	}

	var acquired int
	for i := 0; i < 10; i++ {
		if <-results {
			acquired++
		}
	}
	// Exactly one goroutine should acquire the lock
	assert.Equal(t, 1, acquired, "exactly one goroutine should acquire the lock")
}
