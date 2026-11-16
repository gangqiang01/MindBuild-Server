package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCreateMCPServicePreservesEnabledState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			require.NoError(t, db.AutoMigrate(&types.MCPService{}))
			repo := &mcpServiceRepository{db: db}
			service := &types.MCPService{
				TenantID:      1,
				Name:          "test",
				Enabled:       tc.enabled,
				TransportType: types.MCPTransportSSE,
			}
			require.NoError(t, repo.Create(context.Background(), service))
			require.Equal(t, tc.enabled, service.Enabled)
			stored, err := repo.GetByID(context.Background(), 1, service.ID)
			require.NoError(t, err)
			require.Equal(t, tc.enabled, stored.Enabled)
		})
	}
}

func newMCPRepoDB(t *testing.T) (*gorm.DB, *mcpServiceRepository) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&types.MCPService{}))
	return db, &mcpServiceRepository{db: db}
}

// GetByName resolves an active service by (tenant, name); unknown names and
// other tenants return nil without an error.
func TestGetByName(t *testing.T) {
	_, repo := newMCPRepoDB(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &types.MCPService{
		ID: "svc-1", TenantID: 1, Name: "Redmine",
		TransportType: types.MCPTransportSSE,
	}))

	got, err := repo.GetByName(ctx, 1, "Redmine")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "svc-1", got.ID)

	missing, err := repo.GetByName(ctx, 1, "GitLab")
	require.NoError(t, err)
	require.Nil(t, missing)

	otherTenant, err := repo.GetByName(ctx, 2, "Redmine")
	require.NoError(t, err)
	require.Nil(t, otherTenant)
}

// The (tenant_id, name) unique index is the hard backstop: a second insert
// with the same pair fails, while the same name in another tenant is allowed.
func TestCreateRejectsDuplicateNameInSameTenant(t *testing.T) {
	_, repo := newMCPRepoDB(t)
	ctx := context.Background()
	base := &types.MCPService{TenantID: 1, Name: "Redmine", TransportType: types.MCPTransportSSE}
	require.NoError(t, repo.Create(ctx, base))

	dup := &types.MCPService{TenantID: 1, Name: "Redmine", TransportType: types.MCPTransportSSE}
	require.Error(t, repo.Create(ctx, dup))

	otherTenant := &types.MCPService{TenantID: 2, Name: "Redmine", TransportType: types.MCPTransportSSE}
	require.NoError(t, repo.Create(ctx, otherTenant))
}

// Soft-deleted rows are excluded from GetByName, so a deleted service's name
// becomes reusable.
func TestGetByNameExcludesSoftDeleted(t *testing.T) {
	_, repo := newMCPRepoDB(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &types.MCPService{
		ID: "svc-1", TenantID: 1, Name: "Redmine",
		TransportType: types.MCPTransportSSE,
	}))
	require.NoError(t, repo.Delete(ctx, 1, "svc-1"))

	got, err := repo.GetByName(ctx, 1, "Redmine")
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, repo.Create(ctx, &types.MCPService{
		ID: "svc-2", TenantID: 1, Name: "Redmine",
		TransportType: types.MCPTransportSSE,
	}))
}
