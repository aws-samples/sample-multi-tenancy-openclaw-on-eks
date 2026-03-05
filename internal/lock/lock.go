package lock

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "tenant:waking:"

// Locker manages distributed wake locks via Redis
type Locker interface {
	AcquireWakeLock(ctx context.Context, tenantID string, ttl time.Duration) (bool, error)
	ReleaseWakeLock(ctx context.Context, tenantID string) error
}

// RedisLocker implements Locker using Redis SET NX EX
type RedisLocker struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *RedisLocker {
	return &RedisLocker{rdb: rdb}
}

// AcquireWakeLock tries to acquire an exclusive wake lock for tenantID.
// Returns true if acquired, false if already held by another replica.
func (l *RedisLocker) AcquireWakeLock(ctx context.Context, tenantID string, ttl time.Duration) (bool, error) {
	key := keyPrefix + tenantID
	ok, err := l.rdb.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis SetNX: %w", err)
	}
	return ok, nil
}

// ReleaseWakeLock releases the wake lock for tenantID
func (l *RedisLocker) ReleaseWakeLock(ctx context.Context, tenantID string) error {
	key := keyPrefix + tenantID
	return l.rdb.Del(ctx, key).Err()
}

// MockLocker is an in-memory locker for testing
type MockLocker struct {
	locks map[string]bool
	mu    chan struct{}
}

func NewMock() *MockLocker {
	m := &MockLocker{
		locks: make(map[string]bool),
		mu:    make(chan struct{}, 1),
	}
	m.mu <- struct{}{}
	return m
}

func (m *MockLocker) AcquireWakeLock(_ context.Context, tenantID string, _ time.Duration) (bool, error) {
	<-m.mu
	defer func() { m.mu <- struct{}{} }()
	if m.locks[tenantID] {
		return false, nil
	}
	m.locks[tenantID] = true
	return true, nil
}

func (m *MockLocker) ReleaseWakeLock(_ context.Context, tenantID string) error {
	<-m.mu
	defer func() { m.mu <- struct{}{} }()
	delete(m.locks, tenantID)
	return nil
}
