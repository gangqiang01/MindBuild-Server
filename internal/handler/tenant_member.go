package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// TenantMemberHandler exposes /tenants/:id/members CRUD. The route layer
// enforces RBAC (Viewer for list, Owner for any mutation) — see
// router.RegisterTenantRoutes — so we don't re-check role here.
//
// Tenant scoping: the auth middleware resolves the caller's role against
// the *active* tenant (JWT / X-Tenant-ID switch / API-key). The URL :id
// is independent and MUST be cross-checked: a user who is Owner of
// tenant A could otherwise POST /tenants/B/members and have the role
// gate happily accept their tenant-A role for an operation that targets
// tenant B. That cross-check now lives in
// middleware.RequirePathTenantMatch (mounted at the /tenants/:id route
// group); by the time a request reaches one of the methods below, :id
// is guaranteed to either match the active tenant or carry a
// cross-tenant superuser bypass.
type TenantMemberHandler struct {
	memberService interfaces.TenantMemberService
	userService   interfaces.UserService
}

// NewTenantMemberHandler wires the dependencies. PR 1 already provides
// both services through the dig container; we just consume them. The
// previous *config.Config argument was removed once
// middleware.RequirePathTenantMatch took over the cross-tenant
// superuser carve-out.
func NewTenantMemberHandler(
	memberService interfaces.TenantMemberService,
	userService interfaces.UserService,
) *TenantMemberHandler {
	return &TenantMemberHandler{
		memberService: memberService,
		userService:   userService,
	}
}

// addMemberRequest is the JSON body for POST /tenants/:id/members.
// Email is the user-facing invite identifier; the handler resolves it to a
// User via UserService.GetUserByEmail. PR 3 does not implement
// email-based invitations for users that don't exist yet — the invitee
// must already have an account. Sending an email invite is tracked as a
// PR 4 candidate.
type addMemberRequest struct {
	Email string           `json:"email" binding:"required,email"`
	Role  types.TenantRole `json:"role" binding:"required"`
}

// updateMemberRoleRequest is the JSON body for PUT /tenants/:id/members/:user_id.
type updateMemberRoleRequest struct {
	Role types.TenantRole `json:"role" binding:"required"`
}

// parseTenantIDFromPath reads :id from the gin route and validates it as
// a tenant ID. Returning (0, false) means we already wrote the error to
// the gin context and the caller should `return` immediately.
func parseTenantIDFromPath(c *gin.Context) (uint64, bool) {
	raw := strings.TrimSpace(c.Param("id"))
	if raw == "" {
		c.Error(apperrors.NewValidationError("workspace id is required"))
		return 0, false
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		c.Error(apperrors.NewValidationError("workspace id must be a positive integer"))
		return 0, false
	}
	return v, true
}

// ListMembers godoc
// @Summary      列出空间成员
// @Description  分页返回当前空间内 active 成员（含每位成员的角色、邮箱、头像）；支持 q 按邮箱/用户名筛选
// @Tags         空间成员
// @Produce      json
// @Param        id         path   string  true   "空间 ID"
// @Param        q          query  string  false  "按邮箱/用户名模糊筛选"
// @Param        page       query  int     false  "页码（从 1 起）"  default(1)
// @Param        page_size  query  int     false  "每页数量（最大 100）"  default(20)
// @Success      200  {object}  map[string]interface{}
// @Security     Bearer
// @Router       /tenants/{id}/members [get]
func (h *TenantMemberHandler) ListMembers(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}

	q := strings.TrimSpace(c.Query("q"))
	page, pageSize, ok := parseListPagination(c)
	if !ok {
		return
	}

	members, total, err := h.memberService.ListMembersPage(ctx, tenantID, q, page, pageSize)
	if err != nil {
		logger.Errorf(ctx, "ListMembersPage failed: tenant=%d err=%v", tenantID, err)
		c.Error(apperrors.NewInternalServerError("failed to list members").WithDetails(err.Error()))
		return
	}

	// Hydrate user-facing fields in one batched query. Before this we
	// did N+1 GetUserByID calls; tenants with hundreds of members
	// pressed the user repo hard for no good reason. Failure is
	// best-effort — a transient batch error degrades to "no email /
	// username on this page" rather than dropping rows, so dangling
	// memberships can still be cleaned up by the Owner.
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	usersByID := map[string]*types.User{}
	if u, err := h.userService.GetUsersByIDs(ctx, ids); err == nil {
		usersByID = u
	} else {
		logger.Warnf(ctx, "ListMembers batch user lookup failed: tenant=%d err=%v", tenantID, err)
	}

	resp := make([]types.TenantMemberResponse, 0, len(members))
	for _, m := range members {
		row := types.TenantMemberResponse{
			UserID:    m.UserID,
			Role:      m.Role,
			Status:    m.Status,
			InvitedBy: m.InvitedBy,
			JoinedAt:  m.JoinedAt,
		}
		if u, ok := usersByID[m.UserID]; ok && u != nil {
			row.Email = u.Email
			row.Username = u.Username
			row.Avatar = u.Avatar
		}
		resp = append(resp, row)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"members":   resp,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		},
	})
}

// AddMember godoc
// @Summary      直接添加空间成员（直加路径）
// @Description
//
//	Owner 通过 email 直接把用户作为 active 成员添加进当前空间。
//
//	这是【直加路径】，被加入的用户没有任何确认机会就出现在空间里——
//	保留它是为了三类不需要走邀请确认的场景：
//	  1. 自动化脚本 / 平台运维 / 数据迁移；
//	  2. 跨空间超管 (CanAccessAllTenants) 的批量编排；
//	  3. 对接外部 IdP 时由身份源单向同步成员。
//
//	所有由 UI 触发的「邀请伙伴加入」交互应改走
//	POST /tenants/:id/invitations，那条路径会先创建 pending 行，让被邀请
//	人在 /me/invitations 主动接受后再写 tenant_members 行（PR #1303 后续）。
//	这条路径与 invitations 路径共存而不互相替代。
//
// @Tags         空间成员
// @Accept       json
// @Produce      json
// @Param        id        path  string                 true  "空间 ID"
// @Param        request   body  addMemberRequest       true  "邀请请求"
// @Success      201  {object}  map[string]interface{}
// @Security     Bearer
// @Router       /tenants/{id}/members [post]
func (h *TenantMemberHandler) AddMember(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}

	var req addMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewValidationError("invalid request body").WithDetails(err.Error()))
		return
	}
	// Defence in depth — service also re-validates, but rejecting early
	// gives the client a better error message than the generic service
	// sentinel-mapped 400.
	if !req.Role.IsValid() {
		c.Error(apperrors.NewValidationError("role must be one of owner/admin/contributor/viewer"))
		return
	}

	user, err := h.userService.GetUserByEmail(ctx, strings.TrimSpace(req.Email))
	if err != nil {
		// ErrUserNotFound is the deliberate "not registered yet" signal;
		// mapping it to 404 lets the UI render "ask them to sign up first"
		// instead of a generic failure.
		if errors.Is(err, apprepo.ErrUserNotFound) {
			c.Error(apperrors.NewNotFoundError(
				"user with this email is not registered; ask them to sign up first"))
			return
		}
		logger.Errorf(ctx, "GetUserByEmail failed: email=%s err=%v",
			secutils.SanitizeForLog(req.Email), err)
		c.Error(apperrors.NewInternalServerError("failed to look up user").WithDetails(err.Error()))
		return
	}

	// Attribute the invite to a human caller only. The X-API-Key auth
	// path attaches a synthetic "system-<tenantID>" user (see
	// types.IsSyntheticUserID); recording that as invited_by would
	// permanently break join-with-users views and any future "who
	// invited whom" UX. Leaving invited_by NULL is the correct fallback
	// — matches the same treatment KB.CreatorID gets in PR 2.
	caller, _ := types.UserIDFromContext(ctx)
	var invitedBy *string
	if caller != "" && !types.IsSyntheticUserID(caller) {
		invitedBy = &caller
	}

	// Add the member and write the 201 / mapped-error response through the
	// shared helper (also used by the invitation auto-accept path).
	addMemberAndRespond(c, ctx, h.memberService, user, tenantID, req.Role, invitedBy)
}

func writeAddMemberError(
	c *gin.Context,
	ctx context.Context,
	user *types.User,
	tenantID uint64,
	err error,
) {
	switch {
	case errors.Is(err, service.ErrInvalidTenantRole):
		c.Error(apperrors.NewValidationError(err.Error()))
	case errors.Is(err, service.ErrAPIKeyCannotAssignOwner):
		c.Error(apperrors.NewForbiddenError(err.Error()))
	case errors.Is(err, service.ErrMembershipAlreadyExists):
		// 409 reads better than 400 here: the request was syntactically
		// fine, the conflict is semantic ("already a member").
		c.Error(apperrors.NewConflictError(err.Error()))
	default:
		logger.Errorf(ctx, "AddMember failed: user=%s tenant=%d err=%v",
			user.ID, tenantID, err)
		c.Error(apperrors.NewInternalServerError("failed to add member").WithDetails(err.Error()))
	}
}

func writeAddMemberSuccess(c *gin.Context, user *types.User, member *types.TenantMember) {
	// Project the freshly added row through the same response shape the
	// list endpoint uses, so the UI can swap "Add Member" UX into the
	// table without an extra round-trip.
	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data": types.TenantMemberResponse{
			UserID:    member.UserID,
			Email:     user.Email,
			Username:  user.Username,
			Avatar:    user.Avatar,
			Role:      member.Role,
			Status:    member.Status,
			InvitedBy: member.InvitedBy,
			JoinedAt:  member.JoinedAt,
		},
	})
}

// addMemberAndRespond calls TenantMemberService.AddMember and writes the
// HTTP response: 201 with a TenantMemberResponse on success, or the service
// sentinel mapped to its HTTP status (400 / 403 / 409 / 500) on error. It
// always writes exactly one response, so the caller MUST return right after.
// Shared by TenantMemberHandler.AddMember and the auto-accept branch of
// TenantInvitationHandler.CreateInvitation so the mapping never drifts.
func addMemberAndRespond(
	c *gin.Context,
	ctx context.Context,
	memberService interfaces.TenantMemberService,
	user *types.User,
	tenantID uint64,
	role types.TenantRole,
	invitedBy *string,
) {
	member, err := memberService.AddMember(ctx, user.ID, tenantID, role, invitedBy)
	if err != nil {
		writeAddMemberError(c, ctx, user, tenantID, err)
		return
	}
	writeAddMemberSuccess(c, user, member)
}

// createMemberRequest is the JSON body for POST /tenants/:id/members/create.
// Unlike addMemberRequest it carries the identity fields needed to provision
// a brand-new account when the email is not registered yet: Username plus an
// optional Password. An absent (nil) password makes the service mint a
// random one that is returned exactly once in the response body.
type createMemberRequest struct {
	Username string           `json:"username" binding:"required,min=2,max=50"`
	Email    string           `json:"email"    binding:"required,email"`
	Role     types.TenantRole `json:"role"     binding:"required"`
	Password *string          `json:"password"`
}

// createMemberResponse is the `data` payload for POST /tenants/:id/members/create.
// UserCreated distinguishes "we minted a new account" from "we bound an
// existing one" so the UI can decide whether the one-time GeneratedPassword
// needs to be revealed. GeneratedPassword is populated exactly once, only
// for a freshly created account with a server-minted password, and is never
// logged or retrievable again.
type createMemberResponse struct {
	Member            types.TenantMemberResponse `json:"member"`
	UserCreated       bool                       `json:"user_created"`
	GeneratedPassword string                     `json:"generated_password,omitempty"`
}

// createMemberSuccessResponse is the 201 envelope for POST /tenants/:id/members/create.
// It wraps createMemberResponse in the standard {success, data} shape shared by the
// rest of the tenant API (see writeAddMemberSuccess), so the frontend can read
// resp.data.member / resp.data.generated_password uniformly.
type createMemberSuccessResponse struct {
	Success bool                 `json:"success" example:"true"`
	Data    createMemberResponse `json:"data"`
}

// CreateMember godoc
// @Summary      创建用户并添加为空间成员（直建路径）
// @Description  Owner 在成员管理里直接建档并绑定角色，替代邮箱邀请流程：
// @Description
//
//   - 邮箱已注册：直接复用该账号并写入指定角色的 active 成员关系，
//     绝不改写其密码（重置密码走系统管理员接口）；
//
//   - 邮箱未注册：按 tenantless 方式新建账号（不另建个人空间，该空间
//     即其首个成员身份，登录时自动落入本空间）；密码缺省时服务端
//     随机生成，仅在响应体 generated_password 字段返回一次。
//
//     身份冲突（邮箱属于一个账号、用户名属于另一个）返回 409；
//     用户已是本空间成员同样返回 409（与 AddMember 一致）。
//
// @Tags         空间成员
// @Accept       json
// @Produce      json
// @Param        id       path  string  true  "空间 ID"
// @Param        request  body  createMemberRequest  true  "创建成员请求"
// @Success      201  {object}  createMemberSuccessResponse
// @Failure      400  {object}  map[string]interface{}  "参数错误 / 密码策略不满足"
// @Failure      409  {object}  map[string]interface{}  "身份冲突 / 已是成员"
// @Security     Bearer
// @Router       /tenants/{id}/members/create [post]
func (h *TenantMemberHandler) CreateMember(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}

	var req createMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewValidationError("invalid request body").WithDetails(err.Error()))
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if !req.Role.IsValid() {
		c.Error(apperrors.NewValidationError("role must be one of owner/admin/contributor/viewer"))
		return
	}

	// 1) Resolve the target account: reuse it when the email is already
	// registered, otherwise provision a fresh tenantless account whose
	// first membership is this tenant (login lands here via
	// homeOrFirstMembershipTenant).
	user, err := h.userService.GetUserByEmail(ctx, req.Email)
	var generatedPassword string
	userCreated := false
	if err != nil {
		if !errors.Is(err, apprepo.ErrUserNotFound) {
			// Any other lookup error (DB down, ...) means we cannot tell
			// whether the account exists, so skip rather than risk a
			// duplicate insert.
			logger.Errorf(ctx, "CreateMember user lookup failed: email=%s err=%v",
				secutils.SanitizeForLog(req.Email), err)
			c.Error(apperrors.NewInternalServerError("failed to look up user").WithDetails(err.Error()))
			return
		}
		user, generatedPassword, err = h.userService.AdminCreateUser(ctx,
			&types.AdminCreateUserRequest{
				Username: req.Username,
				Email:    req.Email,
				Password: req.Password,
			},
			types.TenantProvisioningTenantless)
		switch {
		case err == nil:
			userCreated = true
		case errors.Is(err, service.ErrUserEmailExists), errors.Is(err, service.ErrUserUsernameExists):
			// Sequential-idempotency duplicate: the service already
			// resolved the existing row for us. Bind it; its password
			// is deliberately left untouched.
			if user == nil {
				logger.Errorf(ctx, "CreateMember duplicate without resolved user: email=%s err=%v",
					secutils.SanitizeForLog(req.Email), err)
				c.Error(apperrors.NewInternalServerError("failed to create user").WithDetails(err.Error()))
				return
			}
		case errors.Is(err, service.ErrUsernameTakenByOther):
			// details 标签供前端映射本地化文案：邮箱未注册、仅用户名被占用。
			c.Error(apperrors.NewConflictError(err.Error()).WithDetails("username_taken"))
			return
		case errors.Is(err, service.ErrUserIdentityConflict):
			c.Error(apperrors.NewConflictError(err.Error()).WithDetails("identity_conflict"))
			return
		case errors.Is(err, service.ErrPasswordPolicy):
			c.Error(apperrors.NewValidationError(err.Error()))
			return
		default:
			logger.Errorf(ctx, "CreateMember user provisioning failed: email=%s err=%v",
				secutils.SanitizeForLog(req.Email), err)
			c.Error(apperrors.NewInternalServerError("failed to create user").WithDetails(err.Error()))
			return
		}
	}

	// 2) Bind the role. Reuse the shared sentinel mapping so this path,
	// AddMember and the invitation auto-accept branch never drift.
	caller, _ := types.UserIDFromContext(ctx)
	var invitedBy *string
	if caller != "" && !types.IsSyntheticUserID(caller) {
		invitedBy = &caller
	}

	member, addErr := h.memberService.AddMember(ctx, user.ID, tenantID, req.Role, invitedBy)
	if addErr != nil {
		writeAddMemberError(c, ctx, user, tenantID, addErr)
		return
	}

	logger.Infof(ctx, "CreateMember: user %s (created=%v) bound to tenant %d as %s",
		user.Email, userCreated, tenantID, req.Role)
	c.JSON(http.StatusCreated, createMemberSuccessResponse{
		Success: true,
		Data: createMemberResponse{
			Member: types.TenantMemberResponse{
				UserID:    member.UserID,
				Email:     user.Email,
				Username:  user.Username,
				Avatar:    user.Avatar,
				Role:      member.Role,
				Status:    member.Status,
				InvitedBy: member.InvitedBy,
				JoinedAt:  member.JoinedAt,
			},
			UserCreated:       userCreated,
			GeneratedPassword: generatedPassword,
		},
	})
}

// UpdateMemberRole godoc
// @Summary      修改空间成员角色
// @Description  Owner 修改某位成员在当前空间内的角色；不能将最后一位 Owner 降级
// @Tags         空间成员
// @Accept       json
// @Produce      json
// @Param        id       path  string                  true  "空间 ID"
// @Param        user_id  path  string                  true  "用户 ID"
// @Param        request  body  updateMemberRoleRequest true  "目标角色"
// @Success      200  {object}  handler.SuccessResponse
// @Security     Bearer
// @Router       /tenants/{id}/members/{user_id} [put]
func (h *TenantMemberHandler) UpdateMemberRole(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("user_id"))
	if userID == "" {
		c.Error(apperrors.NewValidationError("user_id is required"))
		return
	}

	var req updateMemberRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewValidationError("invalid request body").WithDetails(err.Error()))
		return
	}
	if !req.Role.IsValid() {
		c.Error(apperrors.NewValidationError("role must be one of owner/admin/contributor/viewer"))
		return
	}

	if err := h.memberService.UpdateRole(ctx, userID, tenantID, req.Role); err != nil {
		switch {
		case errors.Is(err, service.ErrMembershipNotFound):
			c.Error(apperrors.NewNotFoundError("membership not found"))
		case errors.Is(err, service.ErrLastOwner):
			c.Error(apperrors.NewConflictError(err.Error()))
		case errors.Is(err, service.ErrInvalidTenantRole):
			c.Error(apperrors.NewValidationError(err.Error()))
		case errors.Is(err, service.ErrAPIKeyCannotAssignOwner):
			c.Error(apperrors.NewForbiddenError(err.Error()))
		default:
			logger.Errorf(ctx, "UpdateRole failed: user=%s tenant=%d err=%v",
				userID, tenantID, err)
			c.Error(apperrors.NewInternalServerError("failed to update member role").WithDetails(err.Error()))
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// RemoveMember godoc
// @Summary      移除空间成员
// @Description  Owner 将某位成员从当前空间中移除（软删除 tenant_members 行）；不能移除最后一位 Owner
// @Tags         空间成员
// @Produce      json
// @Param        id       path  string  true  "空间 ID"
// @Param        user_id  path  string  true  "用户 ID"
// @Success      200  {object}  handler.SuccessResponse
// @Security     Bearer
// @Router       /tenants/{id}/members/{user_id} [delete]
func (h *TenantMemberHandler) RemoveMember(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("user_id"))
	if userID == "" {
		c.Error(apperrors.NewValidationError("user_id is required"))
		return
	}

	if err := h.memberService.RemoveMember(ctx, userID, tenantID); err != nil {
		switch {
		case errors.Is(err, service.ErrMembershipNotFound):
			c.Error(apperrors.NewNotFoundError("membership not found"))
		case errors.Is(err, service.ErrLastOwner):
			c.Error(apperrors.NewConflictError(err.Error()))
		case errors.Is(err, service.ErrProtectedAdminUser):
			c.Error(apperrors.NewForbiddenError(err.Error()))
		default:
			logger.Errorf(ctx, "RemoveMember failed: user=%s tenant=%d err=%v",
				userID, tenantID, err)
			c.Error(apperrors.NewInternalServerError("failed to remove member").WithDetails(err.Error()))
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// LeaveTenant godoc
// @Summary      退出当前空间
// @Description  调用方主动退出当前空间。等价于以自己的 user_id 调 RemoveMember，
//
//	但不需要 Owner 权限——非 Owner 也可以自助离开。最后一位 Owner 仍然不能离开
//	（需先把其他成员提升为 Owner），由服务层 ErrLastOwner 拦截。
//
// @Tags         空间成员
// @Produce      json
// @Param        id  path  string  true  "空间 ID"
// @Success      200  {object}  handler.SuccessResponse
// @Security     Bearer
// @Router       /tenants/{id}/leave [post]
func (h *TenantMemberHandler) LeaveTenant(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := parseTenantIDFromPath(c)
	if !ok {
		return
	}
	caller, ok := types.UserIDFromContext(ctx)
	if !ok || caller == "" {
		c.Error(apperrors.NewUnauthorizedError("caller user id missing from context"))
		return
	}

	if err := h.memberService.RemoveMember(ctx, caller, tenantID); err != nil {
		switch {
		case errors.Is(err, service.ErrMembershipNotFound):
			c.Error(apperrors.NewNotFoundError("you are not a member of this workspace"))
		case errors.Is(err, service.ErrLastOwner):
			c.Error(apperrors.NewConflictError(err.Error()))
		case errors.Is(err, service.ErrProtectedAdminUser):
			c.Error(apperrors.NewForbiddenError(err.Error()))
		case errors.Is(err, service.ErrProtectedAdminUser):
			c.Error(apperrors.NewForbiddenError(err.Error()))
		default:
			logger.Errorf(ctx, "LeaveTenant failed: user=%s tenant=%d err=%v",
				caller, tenantID, err)
			c.Error(apperrors.NewInternalServerError("failed to leave workspace").WithDetails(err.Error()))
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}
