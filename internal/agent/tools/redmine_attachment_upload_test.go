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

type fakeRedmineFileService struct {
	interfaces.FileService // embed so only GetFile needs implementing
	data                   map[string][]byte
}

func (f *fakeRedmineFileService) GetFile(_ context.Context, path string) (io.ReadCloser, error) {
	if d, ok := f.data[path]; ok {
		return io.NopCloser(bytes.NewReader(d)), nil
	}
	return nil, os.ErrNotExist
}

type fakeMCPCall struct {
	name         string
	args         map[string]interface{}
	approvalArgs map[string]interface{}
}

type fakeRedmineMCPCaller struct {
	calls   []fakeMCPCall
	results map[string]*types.ToolResult
	err     error
	suspend bool
}

func (c *fakeRedmineMCPCaller) Call(
	_ context.Context,
	_ *types.MCPService,
	tool *types.MCPTool,
	raw json.RawMessage,
	rawApproval json.RawMessage,
) (*types.ToolResult, error) {
	var args map[string]interface{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	var approvalArgs map[string]interface{}
	if err := json.Unmarshal(rawApproval, &approvalArgs); err != nil {
		return nil, err
	}
	c.calls = append(c.calls, fakeMCPCall{name: tool.Name, args: args, approvalArgs: approvalArgs})
	if c.err != nil {
		return nil, c.err
	}
	if c.suspend {
		return &types.ToolResult{
			Success: false,
			Suspend: &types.PromptSuspension{PromptID: "approval-1", Kind: types.AgentPromptKindToolApproval},
		}, nil
	}
	return c.results[tool.Name], nil
}

// ---- helpers ----

func newUploadDoc() *types.TemporaryDocument {
	return &types.TemporaryDocument{
		ID:          "doc-1",
		ResourceRef: "minio://tenants/1/doc-1/report.txt",
		FileName:    "report.txt",
		FileType:    ".txt",
		MimeType:    "text/plain",
		FileSize:    12,
	}
}

func newUploadTool(reader interfaces.AttachmentReader, files interfaces.FileService,
	caller redmineMCPToolCaller, services ...*types.MCPService,
) *RedmineAttachmentUploadTool {
	t := NewRedmineAttachmentUploadTool(reader, files, nil, nil, 0, services)
	t.caller = caller
	t.maxBytes = 1024 // tests exercise the cap explicitly without 10 MiB fixtures
	return t
}

func redmineService(id, name string) *types.MCPService {
	return &types.MCPService{
		ID:             id,
		Name:           name,
		Enabled:        true,
		TransportType:  types.MCPTransportHTTPStreamable,
		TemplateConfig: &types.MCPTemplateConfig{Kind: "redmine"},
	}
}

func uploadResultYAML(token string, issueID ...int64) *types.ToolResult {
	body := "  upload:\n    token: " + token + "\n    filename: report.txt\n    content_type: text/plain\n"
	if len(issueID) > 0 {
		body += "  issue_id: 42\n"
	}
	return &types.ToolResult{
		Success: true,
		Output:  "status_code: 201\nbody:\n" + body + "error: \"\"\n",
	}
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
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: raw}}
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: uploadResultYAML("tok-abc"),
	}}
	svc := redmineService("svc-1", "Redmine")
	tool := newUploadTool(reader, files, mcpClient, svc)

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
	approval := mcpClient.calls[0].approvalArgs
	if _, containsPayload := approval["content"]; containsPayload {
		t.Fatal("base64 file content must not appear in the approval prompt")
	}
	if approval["filename"] != "report.txt" || approval["size_bytes"] != float64(len(raw)) {
		t.Fatalf("approval prompt must identify the file without exposing it: %+v", approval)
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
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: raw}}
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: uploadResultYAML("tok-link", 42),
	}}
	tool := newUploadTool(reader, files, mcpClient, redmineService("svc-1", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt","issue_id":42}`)
	if !result.Success {
		t.Fatalf("link flow failed: %s", result.Error)
	}
	if len(mcpClient.calls) != 1 {
		t.Fatalf("expected one approved upload-and-link MCP call, got %d calls", len(mcpClient.calls))
	}
	if mcpClient.calls[0].args["issue_id"] != float64(42) {
		t.Fatalf("issue ID must be included in the approved upload call: %+v", mcpClient.calls[0].args)
	}
	if result.Data["issue_id"] != int64(42) {
		t.Fatalf("structured result should record the linked issue, got %+v", result.Data)
	}
}

func TestRedmineUploadAttachmentPropagatesApprovalSuspension(t *testing.T) {
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: []byte("evidence")}}
	tool := newUploadTool(reader, files, &fakeRedmineMCPCaller{suspend: true}, redmineService("svc-1", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt","issue_id":42}`)

	if result.Suspend == nil || result.Suspend.PromptID != "approval-1" {
		t.Fatalf("approval suspension must propagate to the agent engine: %+v", result)
	}
}

func TestRedmineUploadAttachmentRejectsInvalidIssueID(t *testing.T) {
	caller := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{}}
	tool := newUploadTool(&fakeAttachmentReader{}, &fakeRedmineFileService{}, caller,
		redmineService("svc-1", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt","issue_id":0}`)

	if result.Success || !contains(result.Error, "issue_id must be a positive integer") {
		t.Fatalf("invalid issue ID should be rejected before file access: %+v", result)
	}
	if len(caller.calls) != 0 {
		t.Fatal("invalid issue ID must not call the Redmine MCP service")
	}
}

func TestRedmineUploadAttachmentRequiresIssueLinkConfirmation(t *testing.T) {
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: []byte("evidence")}}
	caller := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: uploadResultYAML("tok-unlinked"),
	}}
	tool := newUploadTool(reader, files, caller, redmineService("svc-1", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt","issue_id":42}`)

	if result.Success || !contains(result.Error, "did not confirm it was linked") {
		t.Fatalf("must not claim success when issue linkage is unconfirmed: %+v", result)
	}
	if result.Data["token"] != "tok-unlinked" {
		t.Fatalf("preserve the created token when issue linkage is unconfirmed: %+v", result.Data)
	}
}

func TestRedmineUploadAttachmentRejectsOversizeBeforeReading(t *testing.T) {
	doc := newUploadDoc()
	doc.FileSize = 2048 // tool cap is 1024
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeRedmineFileService{data: map[string][]byte{}} // any GetFile call would miss
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{}}
	tool := newUploadTool(reader, files, mcpClient, redmineService("s", "Redmine"))

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
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{}}
	tool := newUploadTool(reader, &fakeRedmineFileService{}, mcpClient,
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
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: {Success: false, Error: "Unauthorized"},
	}}
	tool := newUploadTool(reader, files, mcpClient, redmineService("s", "Redmine"))

	result := mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if result.Success || !contains(result.Error, "Unauthorized") {
		t.Fatalf("expected MCP error to surface, got %+v", result)
	}
}

func TestRedmineUploadAttachmentRequiresExplicitServiceWhenAmbiguous(t *testing.T) {
	doc := newUploadDoc()
	reader := &fakeAttachmentReader{doc: &interfaces.AttachmentDocument{Document: doc}}
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: uploadResultYAML("t"),
	}}
	tool := newUploadTool(reader, files, mcpClient,
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
	files := &fakeRedmineFileService{data: map[string][]byte{doc.ResourceRef: []byte("x")}}
	mcpClient := &fakeRedmineMCPCaller{results: map[string]*types.ToolResult{
		redmineUploadContentTool: uploadResultYAML("t"),
	}}
	tool := newUploadTool(reader, files, mcpClient, redmineService("s", "Redmine"))

	_ = mustExecute(t, tool, `{"attachment":"report.txt"}`)
	if got := mcpClient.calls[0].args["content_type"]; got != "application/octet-stream" {
		t.Fatalf("unknown extension must fall back to octet-stream, got %v", got)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
