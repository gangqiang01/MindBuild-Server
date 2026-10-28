// Bootstrap-time hooks that run after the DI container is built but
// before the HTTP server starts listening. These are deliberately
// best-effort: any failure here only warns and does NOT abort startup.
// The reasoning is that an operator running with a misconfigured env
// var should still be able to bring the server up (and fix the issue
// from the running instance) rather than have a typo brick the deploy.
package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// bootstrapEnvVar is the env var that names the email of the user who
// may be promoted to system administrator when the deployment has no
// existing system administrators.
//
// Why an env var (vs a CLI subcommand)?
//   - Zero-friction in docker-compose / k8s deploys: set it once in the
//     manifest and the very first user account that signs up with that
//     email is auto-promoted, with no extra ops step.
//   - Idempotent: if the user is already a system admin, bootstrapping is
//     a no-op.
//   - Safe to leave set: once at least one system admin exists, the env
//     var stops granting privileges. That prevents a UI revoke from being
//     silently undone on the next restart.
const bootstrapEnvVar = "WEKNORA_BOOTSTRAP_SYSTEM_ADMIN_EMAIL"

// runStartupBootstrap consults the env and applies any one-shot
// bootstrap actions. Currently it only handles system-admin promotion;
// future bootstrap steps (default model seeding, etc.) can be added
// here as additional dig.Invoke calls.
func runStartupBootstrap(c *dig.Container) {
	ctx := context.Background()

	// Legacy hash repair for migration 000065 placeholder rows. Invoked each
	// startup but short-circuits with a cheap EXISTS once every row is
	// backfilled (no api_key decryption on the steady-state path).
	if err := c.Invoke(func(apiKeySvc interfaces.TenantAPIKeyService) {
		if n, err := apiKeySvc.BackfillMissingKeyHashes(ctx); err != nil {
			logger.Warnf(ctx, "[bootstrap] tenant api key hash backfill failed: %v", err)
		} else if n > 0 {
			logger.Infof(ctx, "[bootstrap] backfilled %d legacy tenant api key hash(es)", n)
		}
	}); err != nil {
		logger.Warnf(ctx, "[bootstrap] failed to resolve TenantAPIKeyService: %v", err)
	}

	// Seed the configured bootstrap admin account (opt-in via the
	// admin_bootstrap section of config.yaml). Best-effort like the rest
	// of the bootstrap: a failure here warns but never blocks startup.
	// The admin is bound to the shared default workspace (created on first
	// boot if absent) rather than being given a personal one, so its login
	// lands in that workspace.
	if err := c.Invoke(func(
		cfg *config.Config,
		userSvc interfaces.UserService,
		tenantSvc interfaces.TenantService,
		memberSvc interfaces.TenantMemberService,
	) {
		seedBootstrapAdmin(ctx, cfg, userSvc, tenantSvc, memberSvc)
	}); err != nil {
		logger.Warnf(ctx, "[bootstrap] failed to resolve AdminBootstrap dependencies: %v", err)
	}

	email := strings.TrimSpace(os.Getenv(bootstrapEnvVar))
	if email == "" {
		return
	}
	// dig.Invoke resolves UserService from the container; if user
	// service registration is broken we want to know loudly, but still
	// not abort startup — bootstrap is best-effort.
	if err := c.Invoke(func(userSvc interfaces.UserService) {
		bootstrapSystemAdmin(ctx, userSvc, email)
	}); err != nil {
		logger.Warnf(ctx, "[bootstrap] failed to resolve UserService: %v", err)
	}
}

// bootstrapSystemAdmin promotes the user identified by `email` to system
// administrator only when the deployment currently has no system admins.
// The function is idempotent and non-fatal — it warns and returns on
// every error path.
//
// The bootstrap intentionally does NOT create a user when the email is
// not yet registered: account creation is a workflow with side effects
// (password hashing, tenant assignment, audit) that we don't want to
// short-circuit. Operators should sign up normally first, then set the
// env var on the next restart.
func bootstrapSystemAdmin(ctx context.Context, userSvc interfaces.UserService, email string) {
	user, err := userSvc.GetUserByEmail(ctx, email)
	if err != nil {
		// "not found" surfaces as an error in this codebase; treat it
		// gently — operators commonly set the var before the user has
		// signed up. The next restart after registration will succeed.
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: user lookup failed (have they signed up yet?): %v",
			bootstrapEnvVar, email, err)
		return
	}
	if user == nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: no matching user (will retry on next restart)",
			bootstrapEnvVar, email)
		return
	}
	if user.IsSystemAdmin {
		logger.Infof(ctx,
			"[bootstrap] %s=%s: user %s is already a system admin (no-op)",
			bootstrapEnvVar, email, user.ID)
		return
	}
	_, total, err := userSvc.ListSystemAdmins(ctx, 0, 1)
	if err != nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: cannot verify existing system admins, skipping promotion: %v",
			bootstrapEnvVar, email, err)
		return
	}
	if total > 0 {
		logger.Infof(ctx,
			"[bootstrap] %s=%s: %d system admin(s) already exist; not promoting user %s",
			bootstrapEnvVar, email, total, user.ID)
		return
	}
	user.IsSystemAdmin = true
	if err := userSvc.UpdateUser(ctx, user); err != nil {
		logger.Warnf(ctx,
			"[bootstrap] %s=%s: failed to promote user %s: %v",
			bootstrapEnvVar, email, user.ID, err)
		return
	}
	logger.Infof(ctx,
		"[bootstrap] promoted user %s (%s) to system admin via %s",
		user.ID, email, bootstrapEnvVar)
}

// defaultTenantName returns the configured name of the shared default
// workspace, falling back to "Default" when the section leaves it unset.
func defaultTenantName(cfg *config.Config) string {
	const fallback = "Default"
	if cfg != nil && cfg.AdminBootstrap != nil {
		if n := strings.TrimSpace(cfg.AdminBootstrap.DefaultTenantName); n != "" {
			return n
		}
	}
	return fallback
}

// ensureDefaultWorkspace returns the workspace carrying the given name,
// creating it on first boot when it does not exist yet. The lookup is an
// exact (trimmed) name match over the global tenant list; the bootstrap
// runs once at startup, so the O(n) scan is acceptable.
//
// A non-nil error means the existing-workspace scan itself failed (DB down,
// ...) — the caller must fall back rather than risk minting a duplicate.
func ensureDefaultWorkspace(
	ctx context.Context,
	tenantSvc interfaces.TenantService,
	name string,
) (*types.Tenant, error) {
	tenants, err := tenantSvc.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tenants {
		if t != nil && strings.TrimSpace(t.Name) == name {
			return t, nil
		}
	}
	return tenantSvc.CreateTenant(ctx, &types.Tenant{
		Name:        name,
		Description: "Default workspace",
		Status:      "active",
	})
}

// bindAdminToDefaultWorkspace idempotently makes the admin an Owner of the
// default workspace and, in-memory, points its home tenant at it so a login
// resolves straight into that workspace. It does NOT persist — the caller
// owns the single UpdateUser so the fresh-seed path can fold the home change
// and the system-admin promotion into one write. It returns true when the
// in-memory home was changed (and therefore needs persisting).
//
// The password and the system-admin flag are deliberately untouched here:
// those stay under operator control (a UI-side revocation must not be
// silently undone on the next restart).
func bindAdminToDefaultWorkspace(
	ctx context.Context,
	memberSvc interfaces.TenantMemberService,
	admin *types.User,
	defaultTenant *types.Tenant,
) bool {
	if admin == nil || defaultTenant == nil || memberSvc == nil {
		return false
	}
	if _, err := memberSvc.EnsureOwner(ctx, admin.ID, defaultTenant.ID); err != nil {
		logger.Warnf(ctx,
			"[bootstrap] failed to ensure owner membership for admin %s in default workspace %d: %v",
			admin.ID, defaultTenant.ID, err)
		return false
	}
	if admin.TenantID == defaultTenant.ID {
		return false
	}
	admin.TenantID = defaultTenant.ID
	return true
}

// seedBootstrapAdmin ensures the admin account configured in the
// admin_bootstrap section of config.yaml exists and is bound to the shared
// default workspace. On a fresh deployment it registers the account
// WITHOUT a personal workspace, makes it the Owner of the default workspace
// (created on first boot if absent), sets that workspace as the account's
// home, and promotes it to system administrator.
//
// When the account already exists, its password and system-admin flag are
// left as-is (an operator can still rotate the password or revoke
// privileges through the UI without the change being silently undone on the
// next restart); the default-workspace binding is re-asserted on every
// startup so an account seeded by an older build (personal workspace) still
// lands in the shared workspace on login.
//
// If the default workspace cannot be resolved (e.g. no usable storage
// backend), the step degrades to the legacy personal-workspace registration
// so the admin account always exists and can log in.
func seedBootstrapAdmin(
	ctx context.Context,
	cfg *config.Config,
	userSvc interfaces.UserService,
	tenantSvc interfaces.TenantService,
	memberSvc interfaces.TenantMemberService,
) {
	if cfg == nil || cfg.AdminBootstrap == nil || !cfg.AdminBootstrap.Enabled {
		return
	}
	ab := cfg.AdminBootstrap
	email := strings.TrimSpace(ab.Email)
	username := strings.TrimSpace(ab.Username)
	if email == "" || username == "" || ab.Password == "" {
		logger.Warnf(ctx,
			"[bootstrap] admin_bootstrap.enabled=true but email/username/password is incomplete; skipping admin seed")
		return
	}

	name := defaultTenantName(cfg)
	defaultTenant, dwErr := ensureDefaultWorkspace(ctx, tenantSvc, name)
	if dwErr != nil || defaultTenant == nil {
		logger.Warnf(ctx,
			"[bootstrap] could not resolve default workspace %q: %v; seeding admin with a personal workspace instead",
			name, dwErr)
		defaultTenant = nil
	}

	existing, err := userSvc.GetUserByEmail(ctx, email)
	if err == nil && existing != nil {
		if bindAdminToDefaultWorkspace(ctx, memberSvc, existing, defaultTenant) {
			if err := userSvc.UpdateUser(ctx, existing); err != nil {
				logger.Warnf(ctx,
					"[bootstrap] admin %s bound to default workspace but failed to persist its home: %v",
					existing.ID, err)
			}
		}
		logger.Infof(ctx,
			"[bootstrap] admin user %s already exists (id=%s); default-workspace binding ensured", email, existing.ID)
		return
	}
	// "not found" is the expected fresh-deployment case: fall through and
	// create the account. Any other lookup error (DB down, ...) means we
	// cannot tell whether the account exists, so skip rather than risk a
	// duplicate insert.
	if err != nil && !errors.Is(err, service.ErrUserNotFound) {
		logger.Warnf(ctx, "[bootstrap] admin user lookup for %s failed: %v", email, err)
		return
	}

	// Fresh deployment. With the default workspace available the account is
	// registered tenantless and bound as its Owner; without it we fall back
	// to the legacy personal-workspace registration so the account still
	// exists and can log in.
	provisioning := types.TenantProvisioningCreatePersonal
	if defaultTenant != nil && memberSvc != nil {
		provisioning = types.TenantProvisioningTenantless
	}
	user, regErr := userSvc.Register(ctx, &types.RegisterRequest{
		Username:           username,
		Email:              email,
		Password:           ab.Password,
		TenantProvisioning: provisioning,
	})
	if regErr != nil {
		logger.Warnf(ctx, "[bootstrap] failed to create admin user %s: %v", email, regErr)
		return
	}
	if provisioning == types.TenantProvisioningTenantless {
		// EnsureOwner is idempotent and re-asserted on the next startup, so
		// a transient failure here must not roll the account back — doing
		// so would leave the deployment without an admin until manual
		// intervention. The home is only pointed at the default workspace
		// once the membership actually landed; the single UpdateUser below
		// folds that home change and the promotion into one write.
		bindAdminToDefaultWorkspace(ctx, memberSvc, user, defaultTenant)
	}
	user.IsSystemAdmin = true
	if err := userSvc.UpdateUser(ctx, user); err != nil {
		logger.Warnf(ctx,
			"[bootstrap] created admin user %s but failed to persist home/system admin: %v", email, err)
		return
	}
	logger.Infof(ctx,
		"[bootstrap] created system admin user %s (id=%s) from admin_bootstrap config", email, user.ID)
}
