package redmine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// Config is the parsed Redmine connector configuration. Connection secrets
// (base_url, api_key) live in DataSourceConfig.Credentials; everything that
// describes what to sync lives in DataSourceConfig.Settings, the same split
// GitLab uses (credentials vs. settings.projects).
type Config struct {
	BaseURL string
	APIKey  string

	// Projects are Redmine project identifiers (the "identifier" field, e.g.
	// "shop", "shop-backend"), not numeric ids. The connector loops per
	// project so the cursor can resume each one independently.
	Projects []string

	// ContentTypes selects which Redmine objects to sync: "issues", "wiki".
	// Empty defaults to ["issues"].
	ContentTypes []string

	// IncludeJournals embeds each issue's journals (comments + field-change
	// notes) in the rendered Markdown. The list endpoint returns journals only
	// when include=journals is requested, which older Redmine may ignore; in
	// that case issues still sync without the discussion section.
	IncludeJournals bool

	// IncludePrivate syncs is_private issues. Off by default so a service
	// account with broad project access does not leak private tickets into a
	// shared knowledge base.
	IncludePrivate bool

	// IssueStatus is passed through as status_id: "*" (all, including closed),
	// "open", "closed", or a status id. Default "*".
	IssueStatus string

	// CreatedAfter filters issues by created_on >= this ISO date
	// ("2024-01-01"). Empty means no filter.
	CreatedAfter string
}

// parseConfig extracts a Config from a DataSourceConfig. It is tolerant of a
// missing projects selection so Validate can run at the credentials step
// before any resources are chosen; the fetch path re-checks len(Projects).
func parseConfig(ds *types.DataSourceConfig) (*Config, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}
	cfg := &Config{
		BaseURL:         stringField(ds.Credentials, "base_url"),
		APIKey:          stringField(ds.Credentials, "api_key"),
		IncludeJournals: true,
		IssueStatus:     "*",
	}
	if s := stringField(ds.Settings, "issue_status"); s != "" {
		cfg.IssueStatus = s
	}
	cfg.CreatedAfter = stringField(ds.Settings, "created_after")
	cfg.ContentTypes = toStringSlice(ds.Settings["content_types"])
	cfg.Projects = toStringSlice(ds.Settings["projects"])
	cfg.IncludeJournals = boolField(ds.Settings, "include_journals", true)
	cfg.IncludePrivate = boolField(ds.Settings, "include_private", false)
	return cfg, nil
}

// requireProjects validates that at least one project is configured. Called by
// the fetch paths (not Validate) so credential testing can precede selection.
func (cfg *Config) requireProjects() error {
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("%w: at least one project is required", datasource.ErrInvalidConfig)
	}
	return nil
}

// cursor tracks the high-water updated_on per project and content type so an
// incremental sync resumed mid-traversal picks up where it left off. Redmine
// exposes no per-item deletion feed, so the cursor records progress only; a
// future full-reconcile path would compare the full id set against it.
type cursor struct {
	Issues map[string]string `json:"issues"` // project identifier -> ISO8601 updated_on
	Wiki   map[string]string `json:"wiki"`   // project identifier -> ISO8601 updated_on
}

func loadCursor(old *types.SyncCursor) cursor {
	cur := cursor{Issues: map[string]string{}, Wiki: map[string]string{}}
	if old == nil {
		return cur
	}
	// The cursor is round-tripped through ConnectorCursor as a "raw" JSON blob
	// (plus the maps for debug/inspection), mirroring how GitLab persists its
	// per-project commit map.
	if raw, ok := old.ConnectorCursor["raw"]; ok {
		if s, ok := raw.(string); s != "" && ok {
			_ = json.Unmarshal([]byte(s), &cur)
		}
	}
	if cur.Issues == nil {
		cur.Issues = map[string]string{}
	}
	if cur.Wiki == nil {
		cur.Wiki = map[string]string{}
	}
	return cur
}

func redmineCursor(value cursor) *types.SyncCursor {
	raw, _ := json.Marshal(value)
	return &types.SyncCursor{
		LastSyncTime: time.Now().UTC(),
		ConnectorCursor: map[string]interface{}{
			"issues": value.Issues,
			"wiki":   value.Wiki,
			"raw":    string(raw),
		},
	}
}

// ---- Redmine REST API response shapes ----
// Field tags match the JSON returned by Redmine (Rest_api wiki). Pointer time
// fields distinguish "absent" from the zero time, which Redmine omits.

type userResp struct {
	User struct {
		ID    int    `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
}

type project struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Identifier  string     `json:"identifier"`
	Description string     `json:"description"`
	Parent      *parentRef `json:"parent"`
	CreatedOn   *time.Time `json:"created_on"`
	UpdatedOn   *time.Time `json:"updated_on"`
}

// parentRef is the id-only shape Redmine embeds under a project's "parent"
// field. A named type (rather than an anonymous struct) keeps test fixtures
// and the tree builder free of repeated inline struct tags.
type parentRef struct {
	ID int `json:"id"`
}

type projectsResp struct {
	Projects   []project `json:"projects"`
	TotalCount int       `json:"total_count"`
	Offset     int       `json:"offset"`
	Limit      int       `json:"limit"`
}

type namedRef struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type customField struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type attachment struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Description string `json:"description"`
	ContentURL  string `json:"content_url"`
}

type journalDetail struct {
	Property string `json:"property"`
	Name     string `json:"name"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
}

type journal struct {
	ID        int             `json:"id"`
	User      namedRef        `json:"user"`
	Notes     string          `json:"notes"`
	CreatedOn *time.Time      `json:"created_on"`
	Details   []journalDetail `json:"details"`
}

type relation struct {
	ID           int    `json:"id"`
	IssueID      int    `json:"issue_id"`
	IssueToID    int    `json:"issue_to_id"`
	RelationType string `json:"relation_type"`
}

type issue struct {
	ID           int           `json:"id"`
	Subject      string        `json:"subject"`
	Description  string        `json:"description"`
	Project      namedRef      `json:"project"`
	Tracker      namedRef      `json:"tracker"`
	Status       namedRef      `json:"status"`
	Priority     namedRef      `json:"priority"`
	Author       namedRef      `json:"author"`
	AssignedTo   *namedRef     `json:"assigned_to"`
	FixedVersion *namedRef     `json:"fixed_version"`
	Category     *namedRef     `json:"category"`
	CreatedOn    *time.Time    `json:"created_on"`
	UpdatedOn    *time.Time    `json:"updated_on"`
	ClosedOn     *time.Time    `json:"closed_on"`
	IsPrivate    bool          `json:"is_private"`
	CustomFields []customField `json:"custom_fields"`
	Journals     []journal     `json:"journals"`
	Attachments  []attachment  `json:"attachments"`
	Relations    []relation    `json:"relations"`
	Children     []namedRef    `json:"children"`
}

type issuesResp struct {
	Issues     []issue `json:"issues"`
	TotalCount int     `json:"total_count"`
	Offset     int     `json:"offset"`
	Limit      int     `json:"limit"`
}

type wikiPage struct {
	Title     string     `json:"title"`
	UpdatedOn *time.Time `json:"updated_on"`
	CreatedOn *time.Time `json:"created_on"`
	Version   int        `json:"version"`
}

type wikiIndexResp struct {
	WikiPages []wikiPage `json:"wiki_pages"`
}

type wikiPageDetail struct {
	Title       string       `json:"title"`
	Text        string       `json:"text"`
	Format      string       `json:"format"`
	Version     int          `json:"version"`
	Author      namedRef     `json:"author"`
	UpdatedOn   *time.Time   `json:"updated_on"`
	CreatedOn   *time.Time   `json:"created_on"`
	Attachments []attachment `json:"attachments"`
}

type wikiPageResp struct {
	WikiPage wikiPageDetail `json:"wiki_page"`
}

// ---- map[string]interface{} accessors ----
// Settings/Credentials arrive from JSON as interface{} values; these helpers
// read them without panicking on a missing or mistyped field.

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func boolField(m map[string]interface{}, key string, def bool) bool {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	default:
		return def
	}
}

// toStringSlice accepts either a []interface{} of strings or of
// {"identifier": "..."} objects, so the frontend may store projects as bare
// strings or as rows without the connector caring.
func toStringSlice(raw interface{}) []string {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		switch s := v.(type) {
		case string:
			if t := strings.TrimSpace(s); t != "" {
				out = append(out, t)
			}
		case map[string]interface{}:
			if id, _ := s["identifier"].(string); strings.TrimSpace(id) != "" {
				out = append(out, strings.TrimSpace(id))
			}
		}
	}
	return out
}

// ---- time helpers ----

// isoTime renders a pointer time as UTC RFC3339, or "" when absent/zero. The
// cursor stores this form so Redmine's updated_on=>= filter parses it.
func isoTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ptrTime dereferences a pointer time, returning the zero value when nil.
func ptrTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
