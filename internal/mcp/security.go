package mcp

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// ValidateServiceOutboundURLs validates every URL that the MCP transport or
// OAuth discovery flow may contact. It is intentionally called both at
// persistence boundaries and immediately before client construction so stale
// or imported rows cannot bypass the current SSRF policy.
func ValidateServiceOutboundURLs(service *types.MCPService) error {
	if service == nil {
		return fmt.Errorf("MCP service is required")
	}
	if service.URL != nil {
		serviceURL := strings.TrimSpace(*service.URL)
		if serviceURL != "" {
			if err := secutils.ValidateURLForSSRF(serviceURL); err != nil {
				return fmt.Errorf("MCP service URL failed SSRF validation: %w", err)
			}
		}
	}
	if service.AuthConfig != nil {
		metadataURL := strings.TrimSpace(service.AuthConfig.AuthServerMetadataURL)
		if metadataURL != "" {
			if err := secutils.ValidateURLForSSRF(metadataURL); err != nil {
				return fmt.Errorf("MCP OAuth metadata URL failed SSRF validation: %w", err)
			}
		}
	}
	if tc := service.TemplateConfig; tc != nil && tc.Kind == "redmine" {
		// The Go service never dials RedmineBaseURL — the managed mcp-redmine
		// container does, per request via X-Redmine-URL — so SSRF/private-IP
		// checks do not apply. A scheme check does: without http(s) the
		// container's httpx client fails every call with
		// "Request URL is missing an 'http://' or 'https://' protocol".
		redmineURL := strings.TrimSpace(tc.RedmineBaseURL)
		if redmineURL == "" {
			return fmt.Errorf("redmine template requires redmine_base_url")
		}
		parsed, err := url.Parse(redmineURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("redmine_base_url must be a full http(s) URL, got: %s", redmineURL)
		}
	}
	return nil
}
