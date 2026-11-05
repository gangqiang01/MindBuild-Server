package redmine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// Compile-time assertions that Connector satisfies both the base Connector
// interface and the optional StreamingConnector. The service prefers
// FetchStream for streaming connectors; FetchAll and FetchIncremental remain
// required by the base interface and are used by the batch path and tests.
var (
	_ datasource.Connector          = (*Connector)(nil)
	_ datasource.StreamingConnector = (*Connector)(nil)
)

// Connector is the Redmine data source connector. It is stateless: each call
// builds a one-shot client from the data source's encrypted credentials, so
// the registry's single instance is safe to reuse across data sources.
type Connector struct {
	client *client
	base   string
}

// NewConnector returns a stateless Redmine connector.
func NewConnector() *Connector { return &Connector{} }

// Type returns the connector type identifier registered in the metadata
// registry and matched against DataSourceConfig.Type.
func (c *Connector) Type() string { return types.ConnectorTypeRedmine }

// configured builds a per-call connector with a client derived from the data
// source's credentials. Returning a new value (instead of mutating the
// receiver like some connectors do) keeps the shared registry instance free of
// per-sync state.
func (c *Connector) configured(ds *types.DataSourceConfig) (*Connector, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}
	baseURL, _ := ds.Credentials["base_url"].(string)
	apiKey, _ := ds.Credentials["api_key"].(string)
	cl, err := newClient(baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	return &Connector{client: cl, base: cl.baseURL}, nil
}

// Validate verifies the API key by calling /users/current.json and, when
// projects are already configured, that each one resolves to a Redmine project
// the key can see. The project check is best-effort: if /projects.json is
// unavailable (the key may have per-project access without list rights) it is
// skipped, and a bad identifier surfaces a clear error at sync time instead.
func (c *Connector) Validate(ctx context.Context, ds *types.DataSourceConfig) error {
	co, err := c.configured(ds)
	if err != nil {
		return err
	}
	if err := co.client.ping(ctx); err != nil {
		return err
	}
	cfg, err := parseConfig(ds)
	if err != nil || len(cfg.Projects) == 0 {
		// Tolerated: Validate also runs at the credentials step where settings
		// may be incomplete. The fetch path re-checks via requireProjects.
		return nil
	}
	resolve, resolveErr := co.resolveProjectMap(ctx)
	if resolveErr != nil {
		return nil // project list unavailable; defer to sync-time checks
	}
	var unresolved []string
	for _, pid := range cfg.Projects {
		if _, found := resolveID(resolve, pid); !found {
			unresolved = append(unresolved, pid)
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("redmine project(s) not found or no access: %s "+
			"(use the project identifier — the URL slug like \"vision-studio\" "+
			"shown in the project page URL; the display name is also accepted)",
			strings.Join(unresolved, ", "))
	}
	return nil
}

// ListResources returns the project tree visible to the API key. Redmine
// returns every project in one flat list carrying its parent id, so the tree is
// assembled in memory: parent=="" yields the roots, a non-empty parent yields
// that project's subprojects. The connector itself syncs from
// settings.projects, so this listing is provided for the resource-picker API
// and for future UI that prefers a tree over manual identifier entry.
func (c *Connector) ListResources(ctx context.Context, ds *types.DataSourceConfig, parent string) ([]types.Resource, error) {
	co, err := c.configured(ds)
	if err != nil {
		return nil, err
	}
	projects, err := co.client.projects(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int]project, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
	}
	// children maps a parent's identifier to its direct subprojects.
	children := make(map[string][]project)
	for _, p := range projects {
		if p.Parent != nil {
			parentProj, ok := byID[p.Parent.ID]
			if !ok {
				continue
			}
			children[parentProj.Identifier] = append(children[parentProj.Identifier], p)
		}
	}

	if parent == "" {
		out := make([]types.Resource, 0, len(projects))
		for _, p := range projects {
			if p.Parent == nil {
				out = append(out, projectResource(p, co.base, len(children[p.Identifier]) > 0))
			}
		}
		return out, nil
	}
	subs := children[parent]
	out := make([]types.Resource, 0, len(subs))
	for _, p := range subs {
		out = append(out, projectResource(p, co.base, len(children[p.Identifier]) > 0))
	}
	return out, nil
}

// ResolveResourceAncestors is empty: ListResources returns the full project
// tree in one call, so there is no lazily-loaded path to reveal for a
// pre-existing selection.
func (c *Connector) ResolveResourceAncestors(context.Context, *types.DataSourceConfig, []string) ([]string, error) {
	return []string{}, nil
}

// FetchAll performs a full sync (cursor ignored) of every selected project's
// configured content types. It drives the streaming path with a collecting
// handler so the fetch and rendering logic is shared with FetchStream.
func (c *Connector) FetchAll(ctx context.Context, ds *types.DataSourceConfig, _ []string) ([]types.FetchedItem, error) {
	co, err := c.configured(ds)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(ds)
	if err != nil {
		return nil, err
	}
	if err := cfg.requireProjects(); err != nil {
		return nil, err
	}
	var out []types.FetchedItem
	collect := &collectHandler{items: &out}
	if _, err := co.streamAll(ctx, cfg, nil, collect); err != nil {
		return nil, err
	}
	return out, nil
}

// FetchIncremental syncs items updated since the cursor and returns the
// advanced cursor. It is the batch-path counterpart to FetchStream; both share
// streamAll so the keyset/dedup policy is identical.
func (c *Connector) FetchIncremental(ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor) ([]types.FetchedItem, *types.SyncCursor, error) {
	co, err := c.configured(ds)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := parseConfig(ds)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.requireProjects(); err != nil {
		return nil, nil, err
	}
	var out []types.FetchedItem
	collect := &collectHandler{items: &out}
	cursor, err := co.streamAll(ctx, cfg, old, collect)
	if err != nil {
		return nil, nil, err
	}
	return out, cursor, nil
}

// FetchStream is the production sync path: it emits each issue and wiki page as
// it is fetched and checkpoints the per-project cursor after each one
// completes, so a sync that times out mid-project resumes from the last
// checkpoint instead of restarting the project.
func (c *Connector) FetchStream(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	co, err := c.configured(ds)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(ds)
	if err != nil {
		return nil, err
	}
	if err := cfg.requireProjects(); err != nil {
		return nil, err
	}
	return co.streamAll(ctx, cfg, old, h)
}

// streamAll walks every selected project, emitting issues then wiki pages and
// checkpointing after each. The cursor carries a per-project, per-content-type
// high-water updated_on so a resumed sync continues exactly where it stopped.
func (co *Connector) streamAll(
	ctx context.Context, cfg *Config, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	prev := loadCursor(old)
	next := cursor{Issues: map[string]string{}, Wiki: map[string]string{}}
	// Carry forward cursors for projects/content types not touched this run so
	// a partial config change does not reset unrelated high-water marks.
	for k, v := range prev.Issues {
		next.Issues[k] = v
	}
	for k, v := range prev.Wiki {
		next.Wiki[k] = v
	}
	wantIssues, wantWiki := contentFlags(cfg)

	// Resolve user-entered project references to canonical identifiers once for
	// the whole sync. Redmine scopes issues and wiki by identifier (a URL slug
	// like "vision-studio"); users often paste the display name ("Vision
	// Studio"), which the API rejects with 404. Best-effort: if the project list
	// is unavailable, fetches proceed with the raw input and a bad identifier
	// surfaces a clear 404 from streamIssues.
	resolve, resolveErr := co.resolveProjectMap(ctx)
	if resolveErr != nil {
		logger.Warnf(ctx, "[Redmine] project list unavailable, cannot resolve display names to identifiers: %v", resolveErr)
	}

	for _, pid := range cfg.Projects {
		canon, _ := resolveID(resolve, pid)
		if canon != pid {
			logger.Infof(ctx, "[Redmine] resolved project %q to identifier %q", pid, canon)
		}
		if wantIssues {
			nc, err := co.streamIssues(ctx, cfg, canon, prev.Issues[canon], h)
			if err != nil {
				return nil, fmt.Errorf("redmine project %q issues: %w", pid, err)
			}
			next.Issues[canon] = nc
			if err := h.Checkpoint(ctx, redmineCursor(next)); err != nil {
				return nil, err
			}
		}
		if wantWiki {
			nc, err := co.streamWiki(ctx, cfg, canon, prev.Wiki[canon], h)
			if err != nil {
				return nil, fmt.Errorf("redmine project %q wiki: %w", pid, err)
			}
			next.Wiki[canon] = nc
			if err := h.Checkpoint(ctx, redmineCursor(next)); err != nil {
				return nil, err
			}
		}
	}
	return redmineCursor(next), nil
}

// resolveProjectMap builds a lookup from either a project identifier or a
// (lowercased, trimmed) display name to the canonical identifier. The map lets
// streamAll translate a pasted display name to the identifier the API requires
// in a single pass over the project list.
func (co *Connector) resolveProjectMap(ctx context.Context) (map[string]string, error) {
	projects, err := co.client.projects(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(projects)*2)
	for _, p := range projects {
		if p.Identifier != "" {
			m[p.Identifier] = p.Identifier
		}
		if name := strings.ToLower(strings.TrimSpace(p.Name)); name != "" {
			m[name] = p.Identifier
		}
	}
	return m, nil
}

// resolveID returns the canonical identifier for a user-entered project
// reference and whether a match was found. No match (including a nil map when
// the project list was unavailable) returns the input unchanged with found=false,
// so callers can either fall back to the raw input (streamAll) or flag it
// (Validate).
func resolveID(m map[string]string, pid string) (string, bool) {
	if pid == "" {
		return pid, false
	}
	if r, ok := m[pid]; ok {
		return r, true
	}
	if r, ok := m[strings.ToLower(strings.TrimSpace(pid))]; ok {
		return r, true
	}
	return pid, false
}

// streamIssues fetches a project's issues updated since `since` ("" = full
// sync) and emits one FetchedItem per issue.
//
// Pagination strategy: Redmine's /issues.json mutates during a sync (an issue
// updated mid-sync moves to a later position), so offset pagination skips
// items permanently. Instead we keyset by updated_on: each page requests
// updated_on>=highWater with sort=updated_on:asc,id:asc and offset=0, and the
// `seen` set drops items already emitted at the boundary timestamp. When a
// full page returns only already-seen items (a bulk edit pushed more than
// issueListLimit issues to one timestamp), we drain that timestamp block by
// advancing offset — safe because within a fixed-timestamp block sorted by id,
// items only leave if re-edited (which moves them to the newer section where
// they are emitted as updates) and never shift in a way that skips an unseen
// item. The hop cap bounds a broken instance.
func (co *Connector) streamIssues(
	ctx context.Context, cfg *Config, pid, since string, h datasource.StreamHandler,
) (string, error) {
	seen := make(map[int]bool)
	var startTime time.Time
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			startTime = t.Add(-incrementalBacktrack)
		}
	}
	startGE := ""
	if !startTime.IsZero() {
		startGE = startTime.UTC().Format(time.RFC3339)
	}
	var maxUpdated time.Time
	offset := 0
	dupPages := 0

	for {
		page, err := co.client.issuesPage(ctx, pid, startGE, offset, cfg)
		if err != nil {
			// A 404 on the first page means the project identifier does not
			// resolve (or the key lacks access). Paginating past a 404 is
			// pointless, so surface a clear message instead of the raw status.
			if offset == 0 && isNotFound(err) {
				return cursorTime(maxUpdated, since), fmt.Errorf("project %q not found or the API key lacks access "+
					"(use the identifier — the URL slug like \"vision-studio\" from the project page URL)", pid)
			}
			return cursorTime(maxUpdated, since), err
		}
		emitted := 0
		for i := range page {
			iss := &page[i]
			if seen[iss.ID] {
				continue
			}
			seen[iss.ID] = true
			if iss.IsPrivate && !cfg.IncludePrivate {
				continue
			}
			if iss.UpdatedOn != nil && (maxUpdated.IsZero() || iss.UpdatedOn.After(maxUpdated)) {
				maxUpdated = *iss.UpdatedOn
			}
			item, err := co.issueItem(ctx, iss, pid, cfg)
			if err != nil {
				return cursorTime(maxUpdated, since), err
			}
			if err := h.Emit(ctx, item); err != nil {
				return cursorTime(maxUpdated, since), err
			}
			emitted++
		}

		if len(page) < issueListLimit {
			return cursorTime(maxUpdated, since), nil
		}
		if emitted > 0 {
			// Re-query from the new high-water mark; seen drops boundary dups.
			offset = 0
			if !maxUpdated.IsZero() {
				startGE = maxUpdated.UTC().Format(time.RFC3339)
			}
			dupPages = 0
			continue
		}
		// Full page, all seen: drain the same-timestamp block by offset.
		offset += issueListLimit
		dupPages++
		if dupPages > maxIssuePaginationHops {
			return cursorTime(maxUpdated, since), fmt.Errorf("pagination stuck (>%d duplicate pages) for project %q", maxIssuePaginationHops, pid)
		}
	}
}

// streamWiki fetches a project's wiki pages updated since `since`. The wiki
// index is not paginated; each changed page's body is fetched individually.
// A 404 on the index means the project has no wiki module enabled and is
// treated as "nothing to sync".
func (co *Connector) streamWiki(
	ctx context.Context, cfg *Config, pid, since string, h datasource.StreamHandler,
) (string, error) {
	var sinceTime time.Time
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			sinceTime = t
		}
	}
	pages, err := co.client.wikiIndex(ctx, pid)
	if err != nil {
		if isNotFound(err) {
			logger.Infof(ctx, "[Redmine] project %q has no wiki module; skipping", pid)
			return since, nil
		}
		return since, err
	}
	var maxUpdated time.Time
	for _, wp := range pages {
		if !sinceTime.IsZero() && wp.UpdatedOn != nil && wp.UpdatedOn.Before(sinceTime) {
			continue
		}
		detail, err := co.client.wikiPage(ctx, pid, wp.Title)
		if err != nil {
			if isNotFound(err) {
				continue // page deleted between index and detail fetch
			}
			return cursorTime(maxUpdated, since), err
		}
		if detail.UpdatedOn != nil && (maxUpdated.IsZero() || detail.UpdatedOn.After(maxUpdated)) {
			maxUpdated = *detail.UpdatedOn
		}
		item, err := co.wikiItem(detail, pid)
		if err != nil {
			return cursorTime(maxUpdated, since), err
		}
		if err := h.Emit(ctx, item); err != nil {
			return cursorTime(maxUpdated, since), err
		}
	}
	return cursorTime(maxUpdated, since), nil
}

// issueItem builds the FetchedItem for one issue. ExternalID is namespaced by
// the instance base URL so two Redmine data sources never collide. Image
// attachments are downloaded with the API key and inlined as base64 data URIs
// so screenshots render when the bug is viewed — Redmine attachment URLs need
// auth the KB viewer does not have. A failed or oversized image falls back to
// a link and never aborts the sync; the issue text is still valuable.
func (co *Connector) issueItem(ctx context.Context, iss *issue, pid string, cfg *Config) (types.FetchedItem, error) {
	images := make(map[int]string, len(iss.Attachments))
	for _, a := range iss.Attachments {
		if !strings.HasPrefix(a.ContentType, "image/") {
			continue
		}
		if int64(a.Size) > maxAttachmentBytes {
			logger.Warnf(ctx, "[Redmine] issue #%d: image %s is %s, exceeds %d; leaving as link",
				iss.ID, a.Filename, humanSize(a.Size), maxAttachmentBytes)
			continue
		}
		dataURI, err := co.client.inlineImage(ctx, a.ContentURL, a.ContentType)
		if err != nil {
			logger.Warnf(ctx, "[Redmine] issue #%d: failed to inline image %s: %v", iss.ID, a.Filename, err)
			continue
		}
		images[a.ID] = dataURI
	}
	title, body := renderIssue(iss, co.base, pid, cfg.IncludeJournals, images)
	meta := map[string]string{
		"channel":            types.ConnectorTypeRedmine,
		"source_type":        "redmine",
		"redmine_project":    pid,
		"redmine_issue_id":   fmt.Sprintf("%d", iss.ID),
		"redmine_tracker":    iss.Tracker.Name,
		"redmine_status":     iss.Status.Name,
		"redmine_assignee":   assigneeName(iss.AssignedTo),
		"redmine_updated_on": isoTime(iss.UpdatedOn),
		"redmine_url":        issueURL(co.base, iss.ID),
	}
	return types.FetchedItem{
		ExternalID:       fmt.Sprintf("redmine:%s:issue:%d", co.base, iss.ID),
		Title:            title,
		Content:          []byte(body),
		ContentType:      "text/markdown",
		FileName:         fmt.Sprintf("issues/%d.md", iss.ID),
		URL:              issueURL(co.base, iss.ID),
		UpdatedAt:        ptrTime(iss.UpdatedOn),
		CreatedAt:        ptrTime(iss.CreatedOn),
		SourceResourceID: pid,
		Metadata:         meta,
	}, nil
}

// wikiItem builds the FetchedItem for one wiki page.
func (co *Connector) wikiItem(p *wikiPageDetail, pid string) (types.FetchedItem, error) {
	title, body := renderWikiPage(p, co.base, pid)
	return types.FetchedItem{
		ExternalID:       fmt.Sprintf("redmine:%s:wiki:%s:%s", co.base, pid, p.Title),
		Title:            title,
		Content:          []byte(body),
		ContentType:      "text/markdown",
		FileName:         fmt.Sprintf("wiki/%s.md", sanitizeFileName(p.Title)),
		URL:              wikiURL(co.base, pid, p.Title),
		UpdatedAt:        ptrTime(p.UpdatedOn),
		CreatedAt:        ptrTime(p.CreatedOn),
		SourceResourceID: pid,
		Metadata: map[string]string{
			"channel":            types.ConnectorTypeRedmine,
			"source_type":        "redmine",
			"redmine_project":    pid,
			"redmine_wiki_title": p.Title,
			"redmine_url":        wikiURL(co.base, pid, p.Title),
		},
	}, nil
}

// ---- helpers ----

// contentFlags parses the configured content types, defaulting to issues only.
func contentFlags(cfg *Config) (issues, wiki bool) {
	for _, ct := range cfg.ContentTypes {
		switch strings.ToLower(strings.TrimSpace(ct)) {
		case "issues", "issue":
			issues = true
		case "wiki":
			wiki = true
		}
	}
	if !issues && !wiki {
		issues = true
	}
	return
}

// cursorTime returns the ISO cursor for the next sync: the max updated_on seen
// this run, or the prior cursor if nothing changed. Keeping the prior cursor on
// an empty run avoids regressing the high-water mark (which would re-scan the
// whole project next time) while a zero-result full sync correctly returns "".
func cursorTime(max time.Time, prev string) string {
	if max.IsZero() {
		return prev
	}
	return max.UTC().Format(time.RFC3339)
}

func assigneeName(ref *namedRef) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// sanitizeFileName maps a wiki title to a safe file name segment. Redmine wiki
// titles may contain spaces and slashes; the KB import path treats FileName as
// a relative path, so slashes outside our intended layout could create stray
// directories.
func sanitizeFileName(title string) string {
	s := strings.TrimSpace(title)
	if s == "" {
		s = "untitled"
	}
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// projectResource maps a Redmine project to a picker Resource. The identifier
// (not the numeric id) is the ExternalID because it is stable, human-readable,
// and what the connector syncs by.
func projectResource(p project, base string, hasChildren bool) types.Resource {
	return types.Resource{
		ExternalID:  p.Identifier,
		Name:        p.Name,
		Type:        "project",
		Description: p.Description,
		URL:         strings.TrimRight(base, "/") + "/projects/" + p.Identifier,
		HasChildren: hasChildren,
	}
}

// collectHandler is a StreamHandler that buffers every emitted item into a
// slice. FetchAll and FetchIncremental use it to share streamAll with the
// streaming path without re-implementing the walk.
type collectHandler struct {
	items *[]types.FetchedItem
}

func (h *collectHandler) Emit(_ context.Context, item types.FetchedItem) error {
	*h.items = append(*h.items, item)
	return nil
}

func (h *collectHandler) Checkpoint(context.Context, *types.SyncCursor) error { return nil }
