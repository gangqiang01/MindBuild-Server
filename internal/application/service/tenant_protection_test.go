package service_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTenantServiceProtectsConfiguredDefaultTenant(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Tenant{}))

	repo := repository.NewTenantRepository(db)
	ctx := context.Background()
	require.NoError(t, repo.CreateTenant(ctx, &types.Tenant{ID: 7, Name: "HQ"}))
	svc := service.NewTenantService(repo, nil, &config.Config{
		AdminBootstrap: &config.AdminBootstrapConfig{DefaultTenantName: " HQ "},
	})

	err = svc.DeleteTenant(ctx, 7)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusConflict, appErr.HTTPCode)
	tenant, err := svc.GetTenantByID(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "HQ", tenant.Name)

	_, err = svc.UpdateTenantProfile(ctx, 7, stringPointer("Renamed"), nil)
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusConflict, appErr.HTTPCode)

	_, err = svc.UpdateTenantProfile(ctx, 7, nil, stringPointer("Updated description"))
	require.NoError(t, err, "the default tenant description should remain editable")
	tenant, err = svc.GetTenantByID(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "HQ", tenant.Name)
	require.Equal(t, "Updated description", tenant.Description)
}

func stringPointer(value string) *string {
	return &value
}
