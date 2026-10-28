-- Revert to the original (non-partial) unique index. NOTE: this reintroduces
-- the re-add-after-removal bug; only use when rolling back 000051.
DROP INDEX IF EXISTS idx_tenant_members_user_tenant_unique;
CREATE UNIQUE INDEX idx_tenant_members_user_tenant_unique
    ON tenant_members(user_id, tenant_id);