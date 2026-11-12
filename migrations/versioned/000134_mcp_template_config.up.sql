-- ============================================================================
-- Migration 000134: Add template_config support for MCP services
--
-- Managed template services (e.g. mcp-redmine) keep their upstream settings
-- here. The Go service does not dial these URLs itself; the MCP client turns
-- them into per-request headers (X-Redmine-URL / X-Redmine-Read-Only) for the
-- shared managed container.
-- ============================================================================

DO $$ BEGIN RAISE NOTICE '[Migration 000134] Adding template_config column to mcp_services...'; END $$;

ALTER TABLE mcp_services ADD COLUMN IF NOT EXISTS template_config JSONB;

DO $$ BEGIN RAISE NOTICE '[Migration 000134] template_config column added to mcp_services'; END $$;
