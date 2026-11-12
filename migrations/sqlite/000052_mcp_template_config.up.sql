-- SQLite counterpart of versioned/000134_mcp_template_config.
-- Managed template services (e.g. mcp-redmine) store per-upstream settings
-- here; the MCP client maps them to per-request X-Redmine-* headers.
ALTER TABLE mcp_services ADD COLUMN template_config TEXT;
