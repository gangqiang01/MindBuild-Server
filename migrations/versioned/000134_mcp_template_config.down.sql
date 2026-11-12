-- Roll back migration 000134: drop the MCP template_config column.
ALTER TABLE mcp_services DROP COLUMN IF EXISTS template_config;
