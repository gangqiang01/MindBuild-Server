-- Rebuild the (user_id, tenant_id) unique index on tenant_members as a
-- PARTIAL index (WHERE deleted_at IS NULL).
--
-- Why: a removed membership is soft-deleted (deleted_at set), but the
-- original index was non-partial, so the soft-deleted row stayed in the
-- index and permanently blocked re-adding the same user to the tenant.
-- SQLite enforces the index itself; the GORM soft-delete scope only filters
-- reads and does NOT affect index enforcement. This brings the SQLite schema
-- in line with the Postgres index created in versioned/000043_tenant_rbac.
DROP INDEX IF EXISTS idx_tenant_members_user_tenant_unique;
CREATE UNIQUE INDEX idx_tenant_members_user_tenant_unique
    ON tenant_members(user_id, tenant_id)
    WHERE deleted_at IS NULL;