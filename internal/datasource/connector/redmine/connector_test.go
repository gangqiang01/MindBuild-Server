package redmine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// fakeRedmine is a minimal Redmine REST API stand-in for connector tests. It
// serves the endpoints the connector hits (users/current, projects, issues,
// wiki index + page) from in-memory state so tests can assert sync behaviour
// without a real instance.
type fakeRedmine struct {
	t        *testing.T
	server   *httptest.Server
	issues   []issue
	pages    []wikiPageDetail
	projects []project
	// attachments maps an attachment download path ("/attachments/<id>") to the
	// bytes the fake serves, so image-inlining can be exercised.
	attachments map[string][]byte
	// issueCalls records the (updated_on>=, offset) of each /issues.json call so
	// the pagination test can assert the keyset re-queries the boundary.
	issueCalls []issueCall
	apiKey     string
}

type issueCall struct {
	gte    string
	offset int
}

func newFakeRedmine(t *testing.T) *fakeRedmine {
	// The connector's SSRF guard blocks loopback by default; allow the
	// httptest server (127.0.0.1) for the duration of each test.
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	secutils.ResetSSRFWhitelistForTest()
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)
	f := &fakeRedmine{t: t, apiKey: "secret-key"}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRedmine) handle(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-Redmine-API-Key"); got != f.apiKey {
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/users/current.json":
		_, _ = fmt.Fprintf(w, `{"user":{"id":1,"login":"svc"}}`)
	case r.URL.Path == "/projects.json":
		var resp projectsResp
		resp.Projects = f.projects
		resp.TotalCount = len(f.projects)
		_ = json.NewEncoder(w).Encode(resp)
	case r.URL.Path == "/issues.json":
		f.handleIssues(w, r)
	case strings.HasSuffix(r.URL.Path, "/wiki/index.json"):
		// /projects/:id/wiki/index.json
		var idx wikiIndexResp
		for _, p := range f.pages {
			idx.WikiPages = append(idx.WikiPages, wikiPage{
				Title: p.Title, Version: p.Version, UpdatedOn: p.UpdatedOn, CreatedOn: p.CreatedOn,
			})
		}
		_ = json.NewEncoder(w).Encode(idx)
	case strings.HasSuffix(r.URL.Path, ".json") && strings.Contains(r.URL.Path, "/wiki/"):
		// /projects/:id/wiki/:title.json
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// ["projects", id, "wiki", "title.json"]
		title := strings.TrimSuffix(strings.Join(segs[3:], "/"), ".json")
		for _, p := range f.pages {
			if p.Title == title {
				_ = json.NewEncoder(w).Encode(wikiPageResp{WikiPage: p})
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(r.URL.Path, "/attachments/"):
		// Attachment download (content_url). Serve a registered blob so image
		// inlining can be tested; the connector downloads these with the API key.
		if blob, ok := f.attachments[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(blob)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleIssues honours updated_on>=, offset and limit=100 with a stable
// (updated_on, id) order, which is what the connector's keyset dedup relies on.
func (f *fakeRedmine) handleIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	gte := ""
	if u := q.Get("updated_on"); strings.HasPrefix(u, ">=") {
		gte = strings.TrimPrefix(u, ">=")
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit := issueListLimit
	f.issueCalls = append(f.issueCalls, issueCall{gte: gte, offset: offset})

	// Redmine returns 404 (not an empty list) when project_id does not resolve
	// to a project the key can see. Simulate that when the fixture knows about
	// projects, so name-resolution is testable; when f.projects is empty the
	// fixture is permissive (the issue-only tests don't set it).
	if len(f.projects) > 0 {
		pid := q.Get("project_id")
		if pid != "" && !f.projectExists(pid) {
			http.NotFound(w, r)
			return
		}
	}

	var filtered []issue
	for _, iss := range f.issues {
		if gte != "" && isoTime(iss.UpdatedOn) < gte {
			continue
		}
		filtered = append(filtered, iss)
	}
	// Stable order by (updated_on, id) — what the connector's keyset dedup
	// assumes Redmine returns.
	sort.SliceStable(filtered, func(i, j int) bool {
		ti, tj := isoTime(filtered[i].UpdatedOn), isoTime(filtered[j].UpdatedOn)
		if ti != tj {
			return ti < tj
		}
		return filtered[i].ID < filtered[j].ID
	})
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[offset:end]
	if page == nil {
		page = []issue{}
	}
	_ = json.NewEncoder(w).Encode(issuesResp{Issues: page, TotalCount: len(filtered), Offset: offset, Limit: limit})
}

// projectExists reports whether pid matches a known project identifier. Redmine
// matches project_id by identifier (or numeric id), never by display name, so
// the fixture intentionally rejects names — that is the 404 the connector's
// name-resolution is meant to sidestep.
func (f *fakeRedmine) projectExists(pid string) bool {
	for _, p := range f.projects {
		if p.Identifier == pid {
			return true
		}
	}
	return false
}

func (f *fakeRedmine) config(contentTypes []string, projects []string, opts ...func(*Config)) *types.DataSourceConfig {
	cfg := &Config{
		BaseURL: f.server.URL, APIKey: f.apiKey,
		Projects: projects, ContentTypes: contentTypes,
		IncludeJournals: true, IssueStatus: "*",
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return &types.DataSourceConfig{
		Type:        types.ConnectorTypeRedmine,
		Credentials: map[string]interface{}{"base_url": cfg.BaseURL, "api_key": cfg.APIKey},
		Settings: map[string]interface{}{
			"projects":         toInterfaceSlice(cfg.Projects),
			"content_types":    toInterfaceSlice(cfg.ContentTypes),
			"include_journals": cfg.IncludeJournals,
			"include_private":  cfg.IncludePrivate,
			"issue_status":     cfg.IssueStatus,
			"created_after":    cfg.CreatedAfter,
		},
	}
}

func toInterfaceSlice(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func withPrivate(b bool) func(*Config) {
	return func(c *Config) { c.IncludePrivate = b }
}

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestValidate(t *testing.T) {
	f := newFakeRedmine(t)
	// Validate now also resolves configured projects against /projects.json, so
	// the fixture must know about "shop" for the good-key path to pass.
	f.projects = []project{{ID: 1, Name: "Shop", Identifier: "shop"}}
	c := NewConnector()

	// Good key.
	if err := c.Validate(context.Background(), f.config(nil, []string{"shop"})); err != nil {
		t.Fatalf("validate good key: %v", err)
	}

	// Bad key: a config whose api_key differs from the server's real key.
	badCfg := f.config(nil, []string{"shop"})
	badCfg.Credentials["api_key"] = "wrong-key"
	if err := c.Validate(context.Background(), badCfg); err == nil {
		t.Fatal("expected validation error for bad key")
	}
}

func TestListResourcesTree(t *testing.T) {
	f := newFakeRedmine(t)
	f.projects = []project{
		{ID: 1, Name: "Shop", Identifier: "shop"},
		{ID: 2, Name: "Shop Backend", Identifier: "shop-backend", Parent: &parentRef{ID: 1}},
	}
	c := NewConnector()

	roots, err := c.ListResources(context.Background(), f.config(nil, nil), "")
	if err != nil {
		t.Fatalf("list roots: %v", err)
	}
	if len(roots) != 1 || roots[0].ExternalID != "shop" || !roots[0].HasChildren {
		t.Fatalf("unexpected roots: %+v", roots)
	}

	subs, err := c.ListResources(context.Background(), f.config(nil, nil), "shop")
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(subs) != 1 || subs[0].ExternalID != "shop-backend" {
		t.Fatalf("unexpected children: %+v", subs)
	}
}

func TestFetchAllIssuesAndWiki(t *testing.T) {
	f := newFakeRedmine(t)
	f.issues = []issue{
		{
			ID: 100, Subject: "Login 500", Description: "occasional 500 after login",
			Project: namedRef{ID: 1, Name: "Shop"}, Tracker: namedRef{ID: 3, Name: "Bug"},
			Status: namedRef{ID: 5, Name: "Resolved"}, Priority: namedRef{ID: 4, Name: "High"},
			Author: namedRef{ID: 11, Name: "zhang"}, AssignedTo: &namedRef{ID: 12, Name: "li"},
			CreatedOn: ts("2026-03-02T09:00:00Z"), UpdatedOn: ts("2026-03-10T10:00:00Z"),
			Journals: []journal{
				{ID: 1, User: namedRef{ID: 12, Name: "li"}, Notes: "reproduced, root cause is X", CreatedOn: ts("2026-03-03T08:00:00Z")},
			},
			Attachments: []attachment{{ID: 9, Filename: "log.txt", ContentType: "text/plain", Size: 2048, ContentURL: f.server.URL + "/attachments/9"}},
		},
	}
	f.pages = []wikiPageDetail{
		{Title: "Welcome", Text: "h2. Intro\n\nWelcome to the shop wiki.", Format: "textile", Version: 3, UpdatedOn: ts("2026-02-01T00:00:00Z")},
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues", "wiki"}, []string{"shop"}), nil)
	if err != nil {
		t.Fatalf("fetch all: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Issue item: title, journals rendered, textile description normalized.
	var issueItem, wikiItem *types.FetchedItem
	for i := range items {
		if items[i].ExternalID == "redmine:"+f.server.URL+":issue:100" {
			issueItem = &items[i]
		}
		if strings.Contains(items[i].ExternalID, ":wiki:shop:Welcome") {
			wikiItem = &items[i]
		}
	}
	if issueItem == nil {
		t.Fatal("issue item not emitted")
	}
	body := string(issueItem.Content)
	if !strings.Contains(body, "[Bug #100]") {
		t.Errorf("issue body missing tracker/id header: %s", body)
	}
	if !strings.Contains(body, "reproduced, root cause is X") {
		t.Errorf("issue body missing journal note: %s", body)
	}
	if !strings.Contains(body, "Status: Resolved") {
		t.Errorf("issue body missing status metadata: %s", body)
	}
	if issueItem.Metadata["redmine_status"] != "Resolved" {
		t.Errorf("issue metadata status mismatch: %q", issueItem.Metadata["redmine_status"])
	}
	if issueItem.URL != f.server.URL+"/issues/100" {
		t.Errorf("issue url mismatch: %q", issueItem.URL)
	}

	// Wiki item: textile heading converted to markdown.
	if wikiItem == nil {
		t.Fatal("wiki item not emitted")
	}
	wbody := string(wikiItem.Content)
	if !strings.Contains(wbody, "## Intro") {
		t.Errorf("wiki body missing converted heading: %s", wbody)
	}
	if !strings.Contains(wbody, "Welcome to the shop wiki.") {
		t.Errorf("wiki body missing text: %s", wbody)
	}
}

func TestFetchIncrementalAdvancesCursor(t *testing.T) {
	f := newFakeRedmine(t)
	f.issues = []issue{
		{ID: 1, Subject: "old", CreatedOn: ts("2025-12-31T00:00:00Z"), UpdatedOn: ts("2025-12-31T00:00:00Z")},
		{ID: 2, Subject: "new", CreatedOn: ts("2026-06-01T00:00:00Z"), UpdatedOn: ts("2026-06-01T00:00:00Z")},
	}
	c := NewConnector()

	// Cursor says last sync was 2026-01-01T00:05:00Z. The backtrack (5 min)
	// means the query starts at ~2025-12-31T23:55:00Z, so issue #1 (updated at
	// 2025-12-31T00:00:00Z) is below the window and excluded by the server;
	// only issue #2 is returned and the cursor advances to its updated_on.
	prev := &types.SyncCursor{ConnectorCursor: map[string]interface{}{
		"raw": `{"issues":{"shop":"2026-01-01T00:05:00Z"},"wiki":{}}`,
	}}
	items, cur, err := c.FetchIncremental(context.Background(), f.config([]string{"issues"}, []string{"shop"}), prev)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "redmine:"+f.server.URL+":issue:2" {
		t.Fatalf("expected only issue #2, got %+v", items)
	}
	if got := cur.ConnectorCursor["issues"].(map[string]string)["shop"]; got != "2026-06-01T00:00:00Z" {
		t.Errorf("cursor did not advance to issue #2 updated_on, got %q", got)
	}
}

// TestPaginationDedupBulkEdit simulates a bulk status change that pushes more
// than issueListLimit issues to the same updated_on second. The keyset must
// drain the timestamp block by offset and emit every issue exactly once.
func TestPaginationDedupBulkEdit(t *testing.T) {
	f := newFakeRedmine(t)
	const total = 150
	f.issues = make([]issue, total)
	for i := 0; i < total; i++ {
		f.issues[i] = issue{
			ID: 1000 + i, Subject: fmt.Sprintf("bulk-%d", i),
			CreatedOn: ts("2026-04-01T00:00:00Z"), UpdatedOn: ts("2026-04-10T12:00:00Z"),
		}
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"shop"}), nil)
	if err != nil {
		t.Fatalf("fetch all bulk: %v", err)
	}
	if len(items) != total {
		t.Errorf("expected %d items, got %d", total, len(items))
	}
	// No duplicate ids.
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.ExternalID] {
			t.Errorf("duplicate external id emitted: %s", it.ExternalID)
		}
		seen[it.ExternalID] = true
	}
	// The keyset must have re-queried the boundary timestamp at least once
	// (otherwise the >100 same-second tail would have been lost).
	reboundary := 0
	for _, call := range f.issueCalls {
		if call.gte == "2026-04-10T12:00:00Z" {
			reboundary++
		}
	}
	if reboundary == 0 {
		t.Errorf("expected the keyset to re-query the bulk-edit timestamp; calls=%+v", f.issueCalls)
	}
}

func TestPrivateIssuesFiltered(t *testing.T) {
	f := newFakeRedmine(t)
	f.issues = []issue{
		{ID: 1, Subject: "public", CreatedOn: ts("2026-01-01T00:00:00Z"), UpdatedOn: ts("2026-01-01T00:00:00Z")},
		{ID: 2, Subject: "secret", IsPrivate: true, CreatedOn: ts("2026-01-01T00:00:00Z"), UpdatedOn: ts("2026-01-01T00:00:00Z")},
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"shop"}), nil)
	if err != nil {
		t.Fatalf("fetch all: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "redmine:"+f.server.URL+":issue:1" {
		t.Fatalf("expected only the public issue, got %+v", items)
	}

	// With include_private on, both sync.
	items, err = c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"shop"}, withPrivate(true)), nil)
	if err != nil {
		t.Fatalf("fetch all private: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected both issues with include_private, got %d", len(items))
	}
}

// TestProjectNameResolution verifies that a user-entered display name is
// resolved to the project identifier via /projects.json before issues are
// fetched. Without resolution, /issues.json?project_id=vision+studio would 404
// (the fixture enforces identifier matching when f.projects is set).
func TestProjectNameResolution(t *testing.T) {
	f := newFakeRedmine(t)
	f.projects = []project{{ID: 7, Name: "Vision Studio", Identifier: "vision-studio"}}
	f.issues = []issue{
		{ID: 50, Subject: "resolved via name", Project: namedRef{ID: 7, Name: "Vision Studio"},
			CreatedOn: ts("2026-01-01T00:00:00Z"), UpdatedOn: ts("2026-01-01T00:00:00Z")},
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"vision studio"}), nil)
	if err != nil {
		t.Fatalf("fetch all with display name: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "redmine:"+f.server.URL+":issue:50" {
		t.Fatalf("expected issue #50 via resolved identifier, got %+v", items)
	}
	// The cursor and the sync must have used the canonical identifier, not the
	// display name, so a later re-sync with the identifier itself continues
	// incrementally rather than re-fetching.
	if items[0].SourceResourceID != "vision-studio" {
		t.Errorf("SourceResourceID should be the resolved identifier, got %q", items[0].SourceResourceID)
	}
}

// TestUnknownProjectClearError checks that a project the key cannot see yields
// a descriptive error rather than a bare "status 404", and that no items are
// emitted.
func TestUnknownProjectClearError(t *testing.T) {
	f := newFakeRedmine(t)
	f.projects = []project{{ID: 1, Name: "Shop", Identifier: "shop"}}
	c := NewConnector()

	_, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"nope"}), nil)
	if err == nil {
		t.Fatal("expected error for unknown project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the bad project, got: %v", err)
	}
}

// TestValidateRejectsUnknownProject checks the create-time path: Validate
// catches an unresolved project before the data source is ever synced.
func TestValidateRejectsUnknownProject(t *testing.T) {
	f := newFakeRedmine(t)
	f.projects = []project{{ID: 1, Name: "Shop", Identifier: "shop"}}
	c := NewConnector()

	err := c.Validate(context.Background(), f.config(nil, []string{"shop", "ghost"}))
	if err == nil {
		t.Fatal("expected Validate to reject unknown project 'ghost'")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the unresolved project, got: %v", err)
	}
	// The known project should still validate on its own.
	if err := c.Validate(context.Background(), f.config(nil, []string{"shop"})); err != nil {
		t.Fatalf("known project should validate: %v", err)
	}
}

// TestIssueImageInlining checks that image attachments are downloaded with the
// API key and inlined as base64 data URIs so screenshots render when the bug is
// viewed (Redmine attachment URLs need auth the KB viewer does not have). A
// non-image attachment stays a link.
func TestIssueImageInlining(t *testing.T) {
	f := newFakeRedmine(t)
	// A real 1x1 PNG, so the data URI is decodable.
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatalf("decode fixture png: %v", err)
	}
	f.attachments = map[string][]byte{"/attachments/42": pngBytes}
	f.issues = []issue{
		{
			ID: 7, Subject: "screenshot bug", Description: "see screenshot",
			CreatedOn: ts("2026-01-01T00:00:00Z"), UpdatedOn: ts("2026-01-01T00:00:00Z"),
			Attachments: []attachment{
				{ID: 42, Filename: "shot.png", ContentType: "image/png", Size: len(pngBytes), ContentURL: f.server.URL + "/attachments/42"},
				{ID: 43, Filename: "notes.txt", ContentType: "text/plain", Size: 10, ContentURL: f.server.URL + "/attachments/43"},
			},
		},
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"shop"}), nil)
	if err != nil {
		t.Fatalf("fetch all: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	body := string(items[0].Content)

	// The image is inlined as a data URI, not left as a remote link.
	if !strings.Contains(body, "![shot.png](data:image/png;base64,") {
		t.Errorf("expected inlined data URI for screenshot, body:\n%s", body)
	}
	if strings.Contains(body, "![shot.png]("+f.server.URL+"/attachments/42)") {
		t.Errorf("image should be inlined, not a remote URL, body:\n%s", body)
	}
	// The non-image attachment stays a link under "## Attachments".
	if !strings.Contains(body, "## Attachments") || !strings.Contains(body, "[notes.txt]") {
		t.Errorf("non-image attachment should remain a link, body:\n%s", body)
	}
}

// TestAttachmentURLRewrite checks that attachment content_urls carrying
// Redmine's internal host (a backend IP the frontend cannot reach) are
// rewritten to the user-configured base_url: the image still inlines (download
// goes through base_url, since the internal host is unreachable) and the
// non-image attachment link uses base_url, not the internal host.
func TestAttachmentURLRewrite(t *testing.T) {
	f := newFakeRedmine(t)
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatalf("decode fixture png: %v", err)
	}
	// Serve the bytes at /attachments/42 on the (reachable) fake server.
	f.attachments = map[string][]byte{"/attachments/42": pngBytes}
	// Redmine reports content_urls with an internal host that is unreachable
	// from neither the connector nor the frontend. Only the path matters; the
	// connector rewrites to base_url + path.
	const internalHost = "http://192.168.99.99:3000"
	f.issues = []issue{
		{
			ID: 9, Subject: "internal host urls", Description: "see screenshot",
			CreatedOn: ts("2026-01-01T00:00:00Z"), UpdatedOn: ts("2026-01-01T00:00:00Z"),
			Attachments: []attachment{
				{ID: 42, Filename: "shot.png", ContentType: "image/png", Size: len(pngBytes), ContentURL: internalHost + "/attachments/42"},
				{ID: 43, Filename: "notes.txt", ContentType: "text/plain", Size: 10, ContentURL: internalHost + "/attachments/43"},
			},
		},
	}
	c := NewConnector()

	items, err := c.FetchAll(context.Background(), f.config([]string{"issues"}, []string{"shop"}), nil)
	if err != nil {
		t.Fatalf("fetch all: %v", err)
	}
	body := string(items[0].Content)

	// The image was downloaded via the rewritten base_url and inlined — proving
	// the connector did NOT try the unreachable internal host.
	if !strings.Contains(body, "![shot.png](data:image/png;base64,") {
		t.Errorf("expected image inlined via rewritten base_url, body:\n%s", body)
	}
	// No rendered URL should carry the internal host.
	if strings.Contains(body, internalHost) {
		t.Errorf("rendered content must not contain the internal host %q, body:\n%s", internalHost, body)
	}
	// The non-image attachment link uses the configured base_url + path.
	want := f.server.URL + "/attachments/43"
	if !strings.Contains(body, "[notes.txt]("+want+")") {
		t.Errorf("non-image link should use base_url %q, body:\n%s", want, body)
	}
}
