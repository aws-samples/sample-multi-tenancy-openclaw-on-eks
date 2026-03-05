package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/shawn/openclaw-tenancy/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRecord(id string) *registry.TenantRecord {
	return &registry.TenantRecord{
		TenantID:     id,
		Status:       registry.StatusIdle,
		Namespace:    "tenants",
		S3Prefix:     "tenants/" + id + "/",
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		IdleTimeoutS: 300,
	}
}

func TestMock_CreateAndGet(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	rec := newRecord("tenant-a")
	require.NoError(t, m.CreateTenant(ctx, rec))

	got, err := m.GetTenant(ctx, "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tenant-a", got.TenantID)
	assert.Equal(t, registry.StatusIdle, got.Status)
}

func TestMock_CreateDuplicate(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	require.NoError(t, m.CreateTenant(ctx, newRecord("dup")))
	err := m.CreateTenant(ctx, newRecord("dup"))
	assert.Error(t, err, "second create should fail")
}

func TestMock_GetNonExistent(t *testing.T) {
	m := registry.NewMock()
	got, err := m.GetTenant(context.Background(), "ghost")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMock_UpdateStatus(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	require.NoError(t, m.CreateTenant(ctx, newRecord("tenant-b")))
	require.NoError(t, m.UpdateStatus(ctx, "tenant-b", registry.StatusRunning, "tenant-b", "10.0.0.1"))

	got, err := m.GetTenant(ctx, "tenant-b")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, got.Status)
	assert.Equal(t, "tenant-b", got.PodName)
	assert.Equal(t, "10.0.0.1", got.PodIP)
}

func TestMock_UpdateActivity(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	rec := newRecord("tenant-c")
	rec.LastActiveAt = time.Now().Add(-5 * time.Minute)
	require.NoError(t, m.CreateTenant(ctx, rec))

	before := time.Now()
	require.NoError(t, m.UpdateActivity(ctx, "tenant-c"))

	got, _ := m.GetTenant(ctx, "tenant-c")
	assert.True(t, got.LastActiveAt.After(before), "last_active_at should be updated")
}

func TestMock_ListIdleTenants(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	// Recent tenant — should NOT appear
	rec1 := newRecord("recent")
	rec1.Status = registry.StatusRunning
	rec1.LastActiveAt = time.Now()
	m.CreateTenant(ctx, rec1)

	// Old tenant — SHOULD appear
	rec2 := newRecord("stale")
	rec2.Status = registry.StatusRunning
	rec2.LastActiveAt = time.Now().Add(-10 * time.Minute)
	m.CreateTenant(ctx, rec2)

	// Idle status tenant — should NOT appear (only running checked)
	rec3 := newRecord("already-idle")
	rec3.Status = registry.StatusIdle
	rec3.LastActiveAt = time.Now().Add(-10 * time.Minute)
	m.CreateTenant(ctx, rec3)

	tenants, err := m.ListIdleTenants(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	assert.Equal(t, "stale", tenants[0].TenantID)
}

func TestMock_DeleteTenant(t *testing.T) {
	m := registry.NewMock()
	ctx := context.Background()

	require.NoError(t, m.CreateTenant(ctx, newRecord("to-delete")))
	require.NoError(t, m.DeleteTenant(ctx, "to-delete"))

	got, err := m.GetTenant(ctx, "to-delete")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMock_DeleteNonExistent(t *testing.T) {
	m := registry.NewMock()
	// Should not error
	assert.NoError(t, m.DeleteTenant(context.Background(), "ghost"))
}
