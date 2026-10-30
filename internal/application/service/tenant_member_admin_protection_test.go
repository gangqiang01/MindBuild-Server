package service_test

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTenantMemberServiceProtectsAdminUsernameFromRemoval(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.User{}, &types.TenantMember{}))

	ctx := context.Background()
	user := &types.User{
		ID:           "admin-user",
		Username:     "admin",
		Email:        "admin@example.com",
		PasswordHash: "hash",
	}
	require.NoError(t, db.Create(user).Error)

	memberRepo := repository.NewTenantMemberRepository(db)
	require.NoError(t, memberRepo.Create(ctx, &types.TenantMember{
		UserID:   user.ID,
		TenantID: 7,
		Role:     types.TenantRoleContributor,
	}))
	svc := service.NewTenantMemberService(memberRepo, nil, repository.NewUserRepository(db), nil)

	err = svc.RemoveMember(ctx, user.ID, 7)
	require.ErrorIs(t, err, service.ErrProtectedAdminUser)

	member, err := memberRepo.Get(ctx, user.ID, 7)
	require.NoError(t, err)
	require.NotNil(t, member, "the protected admin membership must remain active")
}
