package mcp

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestApplyTemplateHeadersRedmine(t *testing.T) {
	t.Run("injects url and read-only headers", func(t *testing.T) {
		headers := map[string]string{}
		applyTemplateHeaders(headers, &types.MCPTemplateConfig{
			Kind:           "redmine",
			RedmineBaseURL: "http://advrm.example.com:3022",
			ReadOnly:       true,
		})
		require.Equal(t, "http://advrm.example.com:3022", headers["X-Redmine-URL"])
		require.Equal(t, "1", headers["X-Redmine-Read-Only"])
	})

	t.Run("omits read-only header when writable", func(t *testing.T) {
		headers := map[string]string{}
		applyTemplateHeaders(headers, &types.MCPTemplateConfig{
			Kind:           "redmine",
			RedmineBaseURL: "https://redmine.example.com",
		})
		require.Equal(t, "https://redmine.example.com", headers["X-Redmine-URL"])
		_, present := headers["X-Redmine-Read-Only"]
		require.False(t, present)
	})

	t.Run("unknown template kind sends nothing", func(t *testing.T) {
		headers := map[string]string{}
		applyTemplateHeaders(headers, &types.MCPTemplateConfig{
			Kind:           "other",
			RedmineBaseURL: "http://should-not-leak.example.com",
		})
		require.Empty(t, headers)
	})

	t.Run("nil config is a no-op", func(t *testing.T) {
		headers := map[string]string{"X-Existing": "keep"}
		applyTemplateHeaders(headers, nil)
		require.Equal(t, map[string]string{"X-Existing": "keep"}, headers)
	})
}

func TestValidateTemplateRedmineBaseURL(t *testing.T) {
	valid := func(raw string) *types.MCPService {
		return &types.MCPService{
			TemplateConfig: &types.MCPTemplateConfig{Kind: "redmine", RedmineBaseURL: raw},
		}
	}

	require.NoError(t, ValidateServiceOutboundURLs(valid("http://advrm.example.com:3022")))
	require.NoError(t, ValidateServiceOutboundURLs(valid("https://redmine.example.com/")))
	// Private/internal hosts must remain allowed: the Go service never dials
	// this URL, the managed mcp-redmine container does.
	require.NoError(t, ValidateServiceOutboundURLs(valid("http://172.21.73.80:8080")))

	require.Error(t, ValidateServiceOutboundURLs(valid("172.21.73.80:8080")),
		"missing protocol must be rejected before the container hits httpx UnsupportedProtocol")
	require.Error(t, ValidateServiceOutboundURLs(valid("ftp://redmine.example.com")))
	require.Error(t, ValidateServiceOutboundURLs(valid("")))

	// Non-redmine templates and absent config are not validated here.
	require.NoError(t, ValidateServiceOutboundURLs(&types.MCPService{
		TemplateConfig: &types.MCPTemplateConfig{Kind: "other"},
	}))
	require.NoError(t, ValidateServiceOutboundURLs(&types.MCPService{}))
}
