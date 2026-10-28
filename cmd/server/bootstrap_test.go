package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// seedUserSvc is a minimal UserService fake for seedBootstrapAdmin tests.
// Unimplemented methods panic via the embedded nil interface — the seed
// step must only call GetUserByEmail / Register / UpdateUser.
type seedUserSvc struct {
	interfaces.UserService
	existing      *types.User
	registerReq   *types.RegisterRequest
	registerCalls int
	updated       *types.User
	updateCalls   int
	lookupErr     error
	registerErr   error
	updateErr     error
}

func (s *seedUserSvc) GetUserByEmail(_ context.Context, _ string) (*types.User, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return s.existing, nil
}

func (s *seedUserSvc) Register(_ context.Context, req *types.RegisterRequest) (*types.User, error) {
	if s.registerErr != nil {
		return nil, s.registerErr
	}
	s.registerCalls++
	s.registerReq = req
	return &types.User{ID: "new-admin-id", Username: req.Username, Email: req.Email}, nil
}

func (s *seedUserSvc) UpdateUser(_ context.Context, user *types.User) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updateCalls++
	s.updated = user
	return nil
}

// seedTenantSvc is a minimal TenantService fake. It supports the two calls
// the seed step makes: ListTenants (to find an existing workspace by name)
// and CreateTenant (to mint the default workspace on first boot).
type seedTenantSvc struct {
	interfaces.TenantService
	tenants     []*types.Tenant
	createdName string
	createCalls int
	nextID      uint64
	listErr     error
	createErr   error
}

func (s *seedTenantSvc) ListTenants(_ context.Context) ([]*types.Tenant, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tenants, nil
}

func (s *seedTenantSvc) CreateTenant(_ context.Context, t *types.Tenant) (*types.Tenant, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.createCalls++
	s.createdName = t.Name
	if s.nextID == 0 {
		s.nextID = 100
	}
	t.ID = s.nextID
	s.nextID++
	s.tenants = append(s.tenants, t)
	return t, nil
}

// seedMemberSvc is a minimal TenantMemberService fake for EnsureOwner.
type seedMemberSvc struct {
	interfaces.TenantMemberService
	ensureOwnerCalls int
	ensuredTenant    uint64
	ensureOwnerErr   error
}

func (s *seedMemberSvc) EnsureOwner(_ context.Context, _ string, tenantID uint64) (*types.TenantMember, error) {
	if s.ensureOwnerErr != nil {
		return nil, s.ensureOwnerErr
	}
	s.ensureOwnerCalls++
	s.ensuredTenant = tenantID
	return &types.TenantMember{TenantID: tenantID, Role: types.TenantRoleOwner}, nil
}

func seedTestConfig() *config.Config {
	return &config.Config{
		AdminBootstrap: &config.AdminBootstrapConfig{
			Enabled:  true,
			Username: "admin",
			Email:    "admin@advantech.com.cn",
			Password: "@dmin123",
		},
	}
}

// A fresh deployment with a resolvable default workspace: the admin is
// registered tenantless (no personal workspace), bound as Owner of the
// default workspace, pointed at it as home, and promoted — all in one write.
func TestSeedBootstrapAdminCreatesUser(t *testing.T) {
	svc := &seedUserSvc{lookupErr: service.ErrUserNotFound}
	tenantSvc := &seedTenantSvc{}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

	if svc.registerCalls != 1 {
		t.Fatalf("Register called %d times, want 1", svc.registerCalls)
	}
	req := svc.registerReq
	if req.Username != "admin" || req.Email != "admin@advantech.com.cn" || req.Password != "@dmin123" {
		t.Fatalf("unexpected register request: %+v", req)
	}
	if req.TenantProvisioning != types.TenantProvisioningTenantless {
		t.Fatalf("provisioning=%q, want tenantless", req.TenantProvisioning)
	}
	// The default workspace is created on first boot under the default name.
	if tenantSvc.createCalls != 1 || tenantSvc.createdName != "Default" {
		t.Fatalf("default workspace creation = (%d, %q), want (1, \"Default\")",
			tenantSvc.createCalls, tenantSvc.createdName)
	}
	// The admin is bound as Owner of the created workspace and homes there.
	if memberSvc.ensureOwnerCalls != 1 || memberSvc.ensuredTenant != tenantSvc.tenants[0].ID {
		t.Fatalf("EnsureOwner = (%d, %d), want (1, %d)",
			memberSvc.ensureOwnerCalls, memberSvc.ensuredTenant, tenantSvc.tenants[0].ID)
	}
	if svc.updateCalls != 1 {
		t.Fatalf("UpdateUser called %d times, want 1", svc.updateCalls)
	}
	if !svc.updated.IsSystemAdmin {
		t.Fatalf("seeded user was not promoted to system admin: %+v", svc.updated)
	}
	if svc.updated.TenantID != tenantSvc.tenants[0].ID {
		t.Fatalf("seeded user home=%d, want default workspace %d",
			svc.updated.TenantID, tenantSvc.tenants[0].ID)
	}
}

// When the default workspace cannot be created (e.g. no usable storage
// backend), the step degrades to the legacy personal-workspace registration
// so the admin account still exists and can log in.
func TestSeedBootstrapAdminFallsBackToPersonalWhenDefaultUnusable(t *testing.T) {
	for name, tenantSvc := range map[string]*seedTenantSvc{
		"list fails":   {listErr: errors.New("db down")},
		"create fails": {createErr: errors.New("no storage backend")},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &seedUserSvc{lookupErr: service.ErrUserNotFound}
			memberSvc := &seedMemberSvc{}
			seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

			if svc.registerCalls != 1 {
				t.Fatalf("Register called %d times, want 1", svc.registerCalls)
			}
			if svc.registerReq.TenantProvisioning != types.TenantProvisioningCreatePersonal {
				t.Fatalf("provisioning=%q, want create_personal fallback", svc.registerReq.TenantProvisioning)
			}
			if memberSvc.ensureOwnerCalls != 0 {
				t.Fatalf("EnsureOwner called %d times, want 0 (no default workspace)", memberSvc.ensureOwnerCalls)
			}
		})
	}
}

// An existing admin is NOT re-registered and is NOT force-promoted; the
// step only re-asserts the default-workspace binding (Owner + home).
func TestSeedBootstrapAdminExistingUserBoundToDefault(t *testing.T) {
	svc := &seedUserSvc{existing: &types.User{ID: "existing", Email: "admin@advantech.com.cn", TenantID: 0}}
	tenantSvc := &seedTenantSvc{tenants: []*types.Tenant{{ID: 100, Name: "Default"}}}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

	if svc.registerCalls != 0 {
		t.Fatalf("Register called %d times for existing user, want 0", svc.registerCalls)
	}
	if tenantSvc.createCalls != 0 {
		t.Fatalf("CreateTenant called %d times, want 0 (workspace already exists)", tenantSvc.createCalls)
	}
	if memberSvc.ensureOwnerCalls != 1 || memberSvc.ensuredTenant != 100 {
		t.Fatalf("EnsureOwner = (%d, %d), want (1, 100)", memberSvc.ensureOwnerCalls, memberSvc.ensuredTenant)
	}
	if svc.updateCalls != 1 || svc.updated.TenantID != 100 {
		t.Fatalf("home not persisted to default workspace: updateCalls=%d home=%d",
			svc.updateCalls, svc.updated.TenantID)
	}
	// The system-admin flag is left untouched (here: still false).
	if svc.updated.IsSystemAdmin {
		t.Fatalf("existing admin's system-admin flag was force-set: %+v", svc.updated)
	}
}

// When the admin already homes in the default workspace, the binding is a
// no-op: EnsureOwner is still called (idempotent) but no write happens.
func TestSeedBootstrapAdminExistingUserAlreadyBound(t *testing.T) {
	svc := &seedUserSvc{existing: &types.User{ID: "existing", Email: "admin@advantech.com.cn", TenantID: 100}}
	tenantSvc := &seedTenantSvc{tenants: []*types.Tenant{{ID: 100, Name: "Default"}}}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

	if svc.registerCalls != 0 {
		t.Fatalf("Register called %d times for existing user, want 0", svc.registerCalls)
	}
	if memberSvc.ensureOwnerCalls != 1 {
		t.Fatalf("EnsureOwner called %d times, want 1 (idempotent re-assert)", memberSvc.ensureOwnerCalls)
	}
	if svc.updateCalls != 0 {
		t.Fatalf("UpdateUser called %d times, want 0 (home already correct)", svc.updateCalls)
	}
}

func TestSeedBootstrapAdminDisabled(t *testing.T) {
	svc := &seedUserSvc{}
	tenantSvc := &seedTenantSvc{}
	memberSvc := &seedMemberSvc{}
	// No AdminBootstrap section at all.
	seedBootstrapAdmin(context.Background(), &config.Config{}, svc, tenantSvc, memberSvc)
	if svc.registerCalls != 0 || tenantSvc.createCalls != 0 || memberSvc.ensureOwnerCalls != 0 {
		t.Fatalf("bootstrap disabled but work was done: register=%d create=%d ensure=%d",
			svc.registerCalls, tenantSvc.createCalls, memberSvc.ensureOwnerCalls)
	}
}

func TestSeedBootstrapAdminIncompleteConfig(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"missing email": {AdminBootstrap: &config.AdminBootstrapConfig{
			Enabled: true, Username: "admin", Password: "p",
		}},
		"missing password": {AdminBootstrap: &config.AdminBootstrapConfig{
			Enabled:  true,
			Username: "admin",
			Email:    "admin@advantech.com.cn",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &seedUserSvc{}
			tenantSvc := &seedTenantSvc{}
			memberSvc := &seedMemberSvc{}
			seedBootstrapAdmin(context.Background(), cfg, svc, tenantSvc, memberSvc)
			if svc.registerCalls != 0 {
				t.Fatalf("Register called %d times, want 0", svc.registerCalls)
			}
			if tenantSvc.createCalls != 0 {
				t.Fatalf("CreateTenant called %d times, want 0", tenantSvc.createCalls)
			}
		})
	}
}

// A non-"not found" lookup error (DB down, ...) means we cannot tell whether
// the account exists, so the step skips registration — and, with the DB
// down, the default workspace is not created either.
func TestSeedBootstrapAdminLookupError(t *testing.T) {
	svc := &seedUserSvc{lookupErr: errors.New("db down")}
	tenantSvc := &seedTenantSvc{listErr: errors.New("db down")}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

	if svc.registerCalls != 0 {
		t.Fatalf("Register called %d times after lookup failure, want 0", svc.registerCalls)
	}
	if tenantSvc.createCalls != 0 {
		t.Fatalf("CreateTenant called %d times after DB failure, want 0", tenantSvc.createCalls)
	}
}

func TestSeedBootstrapAdminRegisterError(t *testing.T) {
	svc := &seedUserSvc{lookupErr: service.ErrUserNotFound, registerErr: errors.New("username taken")}
	tenantSvc := &seedTenantSvc{}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), seedTestConfig(), svc, tenantSvc, memberSvc)

	if svc.updateCalls != 0 {
		t.Fatalf("UpdateUser called %d times after register failure, want 0", svc.updateCalls)
	}
	if memberSvc.ensureOwnerCalls != 0 {
		t.Fatalf("EnsureOwner called %d times after register failure, want 0", memberSvc.ensureOwnerCalls)
	}
}

// A custom default_tenant_name is honoured when creating the workspace.
func TestSeedBootstrapAdminCustomDefaultName(t *testing.T) {
	cfg := seedTestConfig()
	cfg.AdminBootstrap.DefaultTenantName = "HQ"
	svc := &seedUserSvc{lookupErr: service.ErrUserNotFound}
	tenantSvc := &seedTenantSvc{}
	memberSvc := &seedMemberSvc{}
	seedBootstrapAdmin(context.Background(), cfg, svc, tenantSvc, memberSvc)

	if tenantSvc.createdName != "HQ" {
		t.Fatalf("created workspace named %q, want %q", tenantSvc.createdName, "HQ")
	}
}