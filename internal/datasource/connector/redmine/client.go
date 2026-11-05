package redmine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
)

const (
	// maxJSONResponseBytes bounds issue, wiki and project JSON. A Redmine
	// issues page with journals and attachments is small; a larger body means a
	// broken or hostile instance.
	maxJSONResponseBytes int64 = 32 << 20
	// issueListLimit is the page size for /issues.json. Redmine caps limit at
	// 100; requesting more silently returns 100, so we pin it.
	issueListLimit = 100
	// projectListLimit is the page size for /projects.json. The project list is
	// small and mutates slowly, so offset pagination is safe here (unlike
	// issues, which drift under offset during a sync).
	projectListLimit = 100
	// incrementalBacktrack rewinds the updated_on cursor a few minutes so items
	// whose updated_on equals the cursor boundary (same-second edits) are
	// re-fetched and de-duplicated by id instead of being skipped. Without it a
	// burst of updates sharing one timestamp can straddle a sync boundary and
	// the tail is lost until the next full reconcile.
	incrementalBacktrack = 5 * time.Minute
	// maxIssuePaginationHops bounds the duplicate-page drain that handles a
	// bulk edit updating more than issueListLimit issues to the same second. A
	// runaway here means a broken instance advertising full pages of nothing.
	maxIssuePaginationHops = 10000
	// maxAttachmentBytes bounds a single image attachment download. Images up
	// to this size are inlined into the issue Markdown as base64 data URIs so
	// screenshots render when the bug is viewed; larger images stay as links,
	// since a multi-MiB base64 blob would bloat the knowledge store.
	maxAttachmentBytes int64 = 5 << 20 // 5 MiB
)

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limit   int64
}

// newClient builds a Redmine REST client. The base URL is validated against the
// SSRF policy and the HTTP client carries redirect/dial-time SSRF guards, so a
// user-supplied Redmine URL cannot be used to reach internal addresses.
func newClient(baseURL, apiKey string) (*client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("redmine platform configuration is missing")
	}
	if err := datasource.ValidateConnectorBaseURL(baseURL); err != nil {
		return nil, err
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	return &client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    datasource.NewConnectorHTTPClient(30 * time.Second),
		limit:   maxJSONResponseBytes,
	}, nil
}

type apiError struct {
	endpoint string
	status   int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("redmine API %s: status %d", e.endpoint, e.status)
}

// isNotFound reports whether err is a 404 from the Redmine API. Used to treat a
// missing wiki module or a deleted wiki page as "nothing to sync" rather than a
// sync failure.
func isNotFound(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status == http.StatusNotFound
	}
	return false
}

// readCapped reads a response body, refusing anything larger than limit instead
// of buffering it. A truncated body would be indexed as the whole document, so
// an oversized payload is an error rather than a partial ingest.
func readCapped(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds maximum size (%d bytes)", limit)
	}
	return data, nil
}

func (c *client) get(ctx context.Context, endpoint string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	// Redmine accepts the API key either as the X-Redmine-API-Key header or as
	// the key query parameter. The header keeps the secret out of URLs (logs,
	// referrers, proxy caches).
	req.Header.Set("X-Redmine-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{endpoint: endpoint, status: resp.StatusCode}
	}
	body, err := readCapped(resp.Body, c.limit)
	if err != nil {
		return fmt.Errorf("redmine API %s: %w", endpoint, err)
	}
	return json.Unmarshal(body, out)
}

// ping verifies the API key by calling /users/current.json. A 401 surfaces as
// an apiError the service reports as a validation failure.
func (c *client) ping(ctx context.Context) error {
	var u userResp
	return c.get(ctx, "/users/current.json", &u)
}

// projects fetches every project visible to the API key. Redmine returns
// projects as a flat list carrying each parent id, so the caller builds the
// tree in memory; offset pagination is safe because the project set is small
// and stable within a sync.
func (c *client) projects(ctx context.Context) ([]project, error) {
	var all []project
	offset := 0
	for {
		endpoint := fmt.Sprintf("/projects.json?limit=%d&offset=%d&include=", projectListLimit, offset)
		var page projectsResp
		if err := c.get(ctx, endpoint, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Projects...)
		if len(page.Projects) < projectListLimit {
			return all, nil
		}
		offset += projectListLimit
	}
}

// issuesPage fetches one page of issues for a project with updated_on >=
// updatedOnGE ("" for no lower bound) at the given offset. The connector drives
// the keyset loop; this method is a single GET so the dedup/backoff policy
// lives in one place. sort=updated_on:asc,id:asc makes the order deterministic
// within a timestamp, which the drain relies on.
func (c *client) issuesPage(ctx context.Context, projectID, updatedOnGE string, offset int, cfg *Config) ([]issue, error) {
	q := url.Values{}
	q.Set("project_id", projectID)
	q.Set("status_id", cfg.IssueStatus)
	q.Set("sort", "updated_on:asc,id:asc")
	q.Set("limit", strconv.Itoa(issueListLimit))
	q.Set("offset", strconv.Itoa(offset))
	if cfg.IncludeJournals {
		// Modern Redmine returns journals inline on the list endpoint, avoiding
		// an N+1 detail fetch per issue. Older versions ignore the parameter
		// and issues sync without the discussion section.
		q.Set("include", "journals,attachments,relations,children")
	} else {
		// Attachments are always requested so image screenshots can be inlined
		// into the rendered Markdown even when the discussion section is off.
		q.Set("include", "attachments")
	}
	if updatedOnGE != "" {
		q.Set("updated_on", ">="+updatedOnGE)
	}
	if cfg.CreatedAfter != "" {
		q.Set("created_on", ">="+cfg.CreatedAfter)
	}
	var resp issuesResp
	if err := c.get(ctx, "/issues.json?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Issues, nil
}

// wikiIndex lists a project's wiki pages (title + updated_on). The index is not
// paginated; a project wiki is bounded, so this returns the whole set in one
// call and the connector fetches each changed page's body.
func (c *client) wikiIndex(ctx context.Context, projectID string) ([]wikiPage, error) {
	var resp wikiIndexResp
	if err := c.get(ctx, "/projects/"+url.PathEscape(projectID)+"/wiki/index.json", &resp); err != nil {
		return nil, err
	}
	return resp.WikiPages, nil
}

// wikiPage fetches a single wiki page's body and format. Redmine addresses
// pages by title; the title is URL-escaped for the path segment.
func (c *client) wikiPage(ctx context.Context, projectID, title string) (*wikiPageDetail, error) {
	var resp wikiPageResp
	endpoint := "/projects/" + url.PathEscape(projectID) + "/wiki/" + url.PathEscape(title) + ".json"
	if err := c.get(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	return &resp.WikiPage, nil
}

// rewriteAttachmentURL replaces the host of an attachment content_url with the
// user-configured base_url. Redmine generates content_urls from its internal
// `host` setting, which may be a backend IP or internal name the frontend (and
// sometimes the connector) cannot reach; the user-configured base_url is the
// reachable address and is what the user expects to see. The path and query are
// preserved so the link still points at the same attachment. An empty or
// unparseable content_url is returned unchanged (the caller leaves it as a
// link rather than embedding a bad URL).
func rewriteAttachmentURL(contentURL, baseURL string) string {
	if contentURL == "" {
		return ""
	}
	u, err := url.Parse(contentURL)
	if err != nil || u.Path == "" {
		return contentURL
	}
	out := strings.TrimRight(baseURL, "/") + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// downloadAttachment fetches an attachment's bytes using the API key. The
// content_url is rewritten to the configured base_url first (see
// rewriteAttachmentURL), so the download hits the reachable host the user
// configured rather than Redmine's internal host. The base_url was already
// SSRF-validated at client construction, and the SSRF-safe HTTP client adds
// dial/redirect guards on top. The response is capped at maxAttachmentBytes — a
// larger body is rejected rather than partially inlined. Returns the bytes and
// the Content-Type Redmine served (used for the data URI MIME type).
func (c *client) downloadAttachment(ctx context.Context, contentURL string) ([]byte, string, error) {
	fetchURL := rewriteAttachmentURL(contentURL, c.baseURL)
	if fetchURL == "" {
		return nil, "", fmt.Errorf("empty attachment url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Redmine-API-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &apiError{endpoint: fetchURL, status: resp.StatusCode}
	}
	body, err := readCapped(resp.Body, maxAttachmentBytes)
	if err != nil {
		return nil, "", fmt.Errorf("redmine attachment %s: %w", fetchURL, err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// inlineImage downloads an image attachment and returns it as a base64 data URI
// (`data:<mime>;base64,...`) for embedding inline in the issue Markdown, so the
// screenshot renders when the bug is viewed without a fetchable remote URL.
// The Redmine-served Content-Type is preferred; fallbackMIME (the attachment's
// declared content_type) is used when the server omits or reports a non-image
// type.
func (c *client) inlineImage(ctx context.Context, contentURL, fallbackMIME string) (string, error) {
	data, ctype, err := c.downloadAttachment(ctx, contentURL)
	if err != nil {
		return "", err
	}
	mime := strings.TrimSpace(ctype)
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = strings.TrimSpace(fallbackMIME)
	}
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
