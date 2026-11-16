package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---- fakes ----

type fakeAttachmentReader struct {
	doc *interfaces.AttachmentDocument
	err error
}

func (r *fakeAttachmentReader) List(context.Context) ([]interfaces.AttachmentDocument, error) {
	return nil, nil
}

func (r *fakeAttachmentReader) Resolve(context.Context, string, bool) (*interfaces.AttachmentDocument, error) {
	return r.doc, r.err
}

func (r *fakeAttachmentReader) ReadImages(context.Context, string, []int, string, string,
) ([]interfaces.AttachmentImageRead, error) {
	return nil, nil
}

func (r *fakeAttachmentReader) WaitTimeout() time.Duration { return time.Second }

type fakeFileService struct {
	interfaces.FileService // embed so only GetFile needs implementing
	data                   map[string][]byte
}

func (f *fakeFileService) GetFile(_ context.Context, path string) (io.ReadCloser, error) {
	if d, ok := f.data[path]; ok {
		return io.NopCloser(bytes.NewReader(d)), nil
	}
	return nil, os.ErrNotExist
}

type fakeMCPCall struct {
	name string
	args map[string]interface{}
}

type fakeRedmineMCPClient struct {
	mcp.MCPClient // embed so only CallTool needs implementing
	calls         []fakeMCPCall
	results       map[string]*mcp.CallToolResult
	err           error
}

func (c *fakeRedmineMCPClient) CallTool(
	_ context.Context, name string, args map[string]interface{},
) (*mcp.CallToolResult, error) {
	c.calls = append(c.calls, fakeMCPCall{name: name, args: args})
	if c.err != nil {
		return nil, c.err
	}
	return c.results[name], nil
}

type fakeRedmineCaller struct {
	client     mcp.MCPClient
	gotService *types.MCPService
}

func (f *fakeRedmineCaller) GetOrCreateClient(
	_ context.Context, svc *types.MCPService,
) (mcp.MCPClient, error) {
	f.gotService = svc
	return f.client, nil
}

// ---- helpers ----

func newUploadDoc() *types.TemporaryDocument {
	return &types.TemporaryDocument{
		ID:         "doc-1",
		ResourceRef: "minio://tenants/1/doc-1/report.txt",
		FileName:   "report.txt",
		FileType:   ".txt",
		MimeType:   "text/plain",
		FileSize:   12,
	}
}

func newUploadTool(reader interfaces.AttachmentReader, files interfaces.FileService,
	caller mcpToolCaller, services ...*types.MCPService,
) *RedmineAttachmentUploadTool {
	t := NewRedmineAttachmentUploadTool(reader, files, caller, services)
	t.maxBytes = 1024 // tests exercise the cap explicitly without 10 MiB fixtures
	return t
}

func redmineService(id, name string) *types.MCPService {
	return &types.MCPService{
		ID:      id,
		Name:    name,
		Enabled: true,
		TransportType: types.MCPTransportHTTPStreamable,
		TemplateConfig: &types.MCPTemplateConfig{Kind: "redmine"},
	}
}

func uploadResultYAML(token string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.ContentItem{{
		Type: "text",
		Text: "status_code: 201\nbody:\n  upload:\n    token: " + token + "\n    filename: report.txt\n    content_type: text/plain\nerror: \"\"\n",
	}}}
}

func issueLinkResultYAML() *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.ContentItem{{
		Type: "text",
		Text: "status_code: 200\nbody:\n  issue:\n    id: 42\nerror: \"\"\n",
	}}}
}

func mustExecute(t *testing.T, tool *RedmineAttachmentUploadTool, raw string) *types.ToolResult {
	t.Helper()
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatalf("bad fixture json: %v", err)
	}
	args, _ := json.Marshal(input)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	return result
}

// ---- tests ----

func TestRedmineUploadAttachmentEncodesAndUploads(t *testing.T) {
	raw := []byte("hello redmine")
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{
		Attachment: types.MessageAttachment{ID: doc.ID, FileName: doc.FileName},
		Document:   doc,
	}}
	files := &fakeFileService{data: map[string][]byte{doc.ResourceRef: raw}}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{
		redmineUploadContentTool: uploadResultYAML("tok-abc"),
	}}
	caller := &fakeRedmineCaller{client: mcpClient}
	svc := redmineService("svc-1", "Redmine")
	tool := newUploadTool(reader, files, caller, svc)

	result := mustExecute(t, tool, `{"attachment":"attachment://att_deadbeef"}`)
	if !result.Success {
		t.Fatalf("upload failed: %s", result.Error)
	}
	if len(mcpClient.calls) != 1 || mcpClient.calls[0].name != redmineUploadContentTool {
		t.Fatalf("expected one redmine_upload_content call, got %+v", mcpClient.calls)
	}
	args := mcpClient.calls[0].args
	if args["filename"] != "report.txt" || args["content_type"] != "text/plain" {
		t.Fatalf("unexpected upload args: %+v", args)
	}
	gotContent, _ := args["content"].(string)
	if decoded, err := base64.StdEncoding.DecodeString(gotContent); err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("content is not the base64 of the attachment bytes: %v", err)
	}
	if caller.gotService != svc {
		t.Fatal("upload must target the resolved Redmine service")
	}
	if result.Data["token"] != "tok-abc" {
		t.Fatalf("token missing from structured result: %+v", result.Data)
	}
	if _, linked := result.Data["issue_id"]; linked {
		t.Fatal("issue_id must be absent when the caller did not pass one")
	}
}

func TestRedmineUploadAttachmentLinksTokenToIssue(t *testing.T) {
	raw := []byte("bug evidence")
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeFileService{data: map[string][]byte{doc.ResourceRef: raw}}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{
		redmineUploadContentTool: uploadResultYAML("tok-link"),
		redmineRequestTool:       issueLinkResultYAML(),
	}}
	tool := newUploadTool(reader, files, &fakeRedmineCaller{client: mcpClient}, redmineService("svc-1", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt","issue_id":42}`)
	if !result.Success {
		t.Fatalf("link flow failed: %s", result.Error)
	}
	if len(mcpClient.calls) != 2 {
		t.Fatalf("expected upload + issue update, got %d calls", len(mcpClient.calls))
	}
	link := mcpClient.calls[1]
	if link.name != redmineRequestTool || link.args["method"] != "put" || link.args["path"] != "issues/42.json" {
		t.Fatalf("unexpected issue-link call: %+v", link.args)
	}
	data, _ := link.args["data"].(map[string]interface{})
	issue, _ := data["issue"].(map[string]interface{})
	uploads, _ := issue["uploads"].([]map[string]interface{})
	if len(uploads) != 1 || uploads[0]["token"] != "tok-link" ||
		uploads[0]["filename"] != "report.txt" || uploads[0]["content_type"] != "text/plain" {
		t.Fatalf("issue update carries the wrong uploads entry: %+v", data)
	}
	if result.Data["issue_id"] != int64(42) {
		t.Fatalf("structured result should record the linked issue, got %+v", result.Data)
	}
}

func TestRedmineUploadAttachmentRejectsOversizeBeforeReading(t *testing.T) {
	doc := newUploadDoc()
	doc.FileSize = 2048 // tool cap is 1024
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeFileService{data: map[string][]byte{}} // any GetFile call would miss
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{}}
	tool := newUploadTool(reader, files, &fakeRedmineCaller{client: mcpClient}, redmineService("s", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if result.Success || !contains(result.Error, "upload limit") {
		t.Fatalf("expected size-limit refusal, got success=%v error=%q", result.Success, result.Error)
	}
	if len(mcpClient.calls) != 0 {
		t.Fatal("oversized attachment must not reach MCP")
	}
}

func TestRedmineUploadAttachmentRejectsUnauthorizedRef(t *testing.T) {
	reader := &fakeAttachmentReader{err: errors.New("attachment is not in this conversation: nope")}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{}}
	tool := newUploadTool(reader, &fakeFileService{}, &fakeRedmineCaller{client: mcpClient},
		redmineService("s", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"attachment://att_unknown"}`)
	if result.Success || !contains(result.Error, "not in this conversation") {
		t.Fatalf("expected authorization failure, got %+v", result)
	}
	if len(mcpClient.calls) != 0 {
		t.Fatal("unauthorized attachment must not reach MCP")
	}
}

func TestRedmineUploadAttachmentPropagatesMCPError(t *testing.T) {
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{
		redmineUploadContentTool: {
			IsError: true,
			Content: []mcp.ContentItem{{Type: "text", Text: "status_code: 401\nerror: Unauthorized"}},
		},
	}}
	tool := newUploadTool(reader, files, &fakeRedmineCaller{client: mcpClient}, redmineService("s", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if result.Success || !contains(result.Error, "Unauthorized") {
		t.Fatalf("expected MCP error to surface, got %+v", result)
	}
}

func TestRedmineUploadAttachmentRequiresExplicitServiceWhenAmbiguous(t *testing.T) {
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{
		redmineUploadContentTool: uploadResultYAML("t"),
	}}
	tool := newUploadTool(reader, files, &fakeRedmineCaller{client: mcpClient},
		redmineService("s1", "Redmine A"), redmineService("s2", "Redmine B"))

	ambiguous := mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if ambiguous.Success || !contains(ambiguous.Error, "multiple Redmine MCP services") {
		t.Fatalf("expected ambiguity error, got %+v", ambiguous)
	}
	picked := mustExecute(t, tool, `{"attachment":"report.txt","service":"Redmine B"}`)
	if !picked.Success {
		t.Fatalf("explicit service selection failed: %s", picked.Error)
	}
}

func TestRedmineUploadAttachmentMIMEFallback(t *testing.T) {
	doc := newUploadDoc()
	doc.MimeType = ""
	doc.FileType = ".weird"
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPClient{results: map[string]*mcp.CallToolResult{
		redmineUploadContentTool: uploadResultYAML("t"),
	}}
	tool := newUploadTool(reader, files, &fakeRedmineCaller{client: mcpClient}, redmineService("s", "Redmine"))

	_ = mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if got := mcpClient.calls[0].args["content_type"]; got != "application/octet-stream" {
		t.Fatalf("unknown extension must fall back to octet-stream, got %v", got)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
