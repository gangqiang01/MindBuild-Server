-- Roll back SQLite migration 000052.
-- SQLite cannot DROP COLUMN before 3.35; the migration toolchain ships a
-- modern SQLite, and every supported test/runtime version supports it.
ALTER TABLE mcp_services DROP COLUMN template_config;
