package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/stretchr/testify/require"
)

// allowLocalhostURLs puts 127.0.0.1 on the SSRF whitelist for the test and
// restores the previous configuration on cleanup, mirroring the setup used by
// the other MCP service tests.
func allowLocalhostURLs(t *testing.T) {
	t.Helper()
	utils.SetSSRFWhitelistFromRaw("127.0.0.1")
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
}

// newNamedService builds a minimal valid service for the create/update path.
func newNamedService(id string, tenantID uint64, name string) *types.MCPService {
	url := "http://127.0.0.1:8080/mcp"
	return &types.MCPService{
		ID:            id,
		TenantID:      tenantID,
		Name:          name,
		Enabled:       true,
		TransportType: types.MCPTransportHTTPStreamable,
		URL:           &url,
	}
}

// Creating two services with the same name in one tenant must fail with the
// conflict sentinel instead of a generic persistence error.
func TestCreateMCPService_DuplicateNameRejected(t *testing.T) {
	allowLocalhostURLs(t)
	svc, _ := newTestService()
	ctx := context.Background()

	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-1", 1, "Redmine")))

	err := svc.CreateMCPService(ctx, newNamedService("svc-2", 1, "Redmine"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMCPServiceNameConflict), "expected ErrMCPServiceNameConflict, got %v", err)
}

// The same name is allowed in a different tenant because the unique index is
// scoped to (tenant_id, name).
func TestCreateMCPService_SameNameDifferentTenantAllowed(t *testing.T) {
	allowLocalhostURLs(t)
	svc, _ := newTestService()
	ctx := context.Background()

	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-1", 1, "Redmine")))
	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-2", 2, "Redmine")))
}

// Renaming a service onto another service's name is rejected.
func TestUpdateMCPService_RenameCollisionRejected(t *testing.T) {
	allowLocalhostURLs(t)
	svc, _ := newTestService()
	ctx := context.Background()

	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-1", 1, "Redmine")))
	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-2", 1, "GitLab")))

	rename := newNamedService("svc-2", 1, "Redmine")
	err := svc.UpdateMCPService(ctx, rename, map[string]bool{"name": true})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMCPServiceNameConflict), "expected ErrMCPServiceNameConflict, got %v", err)
}

// Re-saving a service under its own name must stay allowed even when the name
// field is explicitly present in the update.
func TestUpdateMCPService_KeepOwnNameAllowed(t *testing.T) {
	allowLocalhostURLs(t)
	svc, repo := newTestService()
	ctx := context.Background()

	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-1", 1, "Redmine")))

	require.NoError(t, svc.UpdateMCPService(ctx, newNamedService("svc-1", 1, "Redmine"), map[string]bool{"name": true}))

	stored, err := repo.GetByID(ctx, 1, "svc-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "Redmine", stored.Name)
}

// A rename to a name no other service uses succeeds.
func TestUpdateMCPService_RenameToUnusedNameAllowed(t *testing.T) {
	allowLocalhostURLs(t)
	svc, _ := newTestService()
	ctx := context.Background()

	require.NoError(t, svc.CreateMCPService(ctx, newNamedService("svc-1", 1, "Redmine")))
	require.NoError(t, svc.UpdateMCPService(ctx, newNamedService("svc-1", 1, "Redmine Prod"), map[string]bool{"name": true}))
}
