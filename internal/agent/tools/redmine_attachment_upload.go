package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gopkg.in/yaml.v3"
)

const (
	// ToolRedmineUploadAttachment is the model-facing name of the Go-side tool
	// that bridges session-authorized chat attachments to the managed
	// mcp-redmine container's redmine_upload_content tool.
	ToolRedmineUploadAttachment = "redmine_upload_attachment"

	// Tool names exposed by the mcp-redmine container (must match server.py).
	redmineUploadContentTool = "redmine_upload_content"
	redmineRequestTool       = "redmine_request"

	// defaultRedmineUploadMaxBytes mirrors mcp-redmine's REDMINE_UPLOAD_MAX_BYTES
	// default (10 MiB). Base64 inflates bytes by ~1.33x inside the JSON tool
	// argument, so the cap deliberately stays below MAX_FILE_SIZE_MB (50 MiB).
	defaultRedmineUploadMaxBytes int64 = 10 * 1024 * 1024
)

// redmineUploadMaxBytes reads WEKNORA_REDMINE_UPLOAD_MAX_MB (deploy-time knob,
// same env-only pattern as internal/utils/filesize.go). The mcp-redmine
// container enforces its own REDMINE_UPLOAD_MAX_BYTES; keep the two aligned.
func redmineUploadMaxBytes() int64 {
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_REDMINE_UPLOAD_MAX_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
			return mb * 1024 * 1024
		}
	}
	return defaultRedmineUploadMaxBytes
}

// mcpToolCaller is the slice of *mcp.MCPManager this tool depends on. It is an
// interface so unit tests inject a fake instead of opening a real connection.
type mcpToolCaller interface {
	GetOrCreateClient(ctx context.Context, service *types.MCPService) (mcp.MCPClient, error)
}

// RedmineAttachmentUploadTool reads a file the user attached to this
// conversation (authorization stays with the session AttachmentReader),
// base64-encodes the bytes and asks the managed mcp-redmine container to upload
// them. The raw bytes and their base64 form never appear in tool descriptions,
// model arguments or logs: the model only names an attachment handle, exactly
// as it does for read_file.
type RedmineAttachmentUploadTool struct {
	reader   interfaces.AttachmentReader
	files    interfaces.FileService
	caller   mcpToolCaller
	services []*types.MCPService // enabled template_config.kind == "redmine" services
	maxBytes int64
}

func NewRedmineAttachmentUploadTool(
	reader interfaces.AttachmentReader,
	files interfaces.FileService,
	caller mcpToolCaller,
	services []*types.MCPService,
) *RedmineAttachmentUploadTool {
	return &RedmineAttachmentUploadTool{
		reader:   reader,
		files:    files,
		caller:   caller,
		services: services,
		maxBytes: redmineUploadMaxBytes(),
	}
}

func (t *RedmineAttachmentUploadTool) Name() string { return ToolRedmineUploadAttachment }

func (t *RedmineAttachmentUploadTool) Description() string {
	return "Upload a file attached to this conversation to Redmine as an attachment token. " +
		"Arguments name the attachment only (the attachment:// handle or file name shown by read_file); " +
		"the file bytes are read server-side through the same session authorization as read_file and " +
		"never exposed in the conversation. Pass issue_id to link the uploaded token to that bug in one " +
		"step (the Redmine two-phase flow: upload then PUT issue uploads). Without issue_id the returned " +
		"token must be attached to an issue yourself via the Redmine MCP redmine_request tool: " +
		`PUT /issues/{id}.json with {"issue": {"uploads": [{"token", "filename", "content_type"}]}}.`
}

func (t *RedmineAttachmentUploadTool) Parameters() json.RawMessage {
	// Raw string literal: inner quotes must stay valid JSON, so examples use
	// single quotes rather than escaped double quotes.
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "attachment": {
      "type": "string",
      "description": "The attachment to upload: its attachment:// handle (e.g. 'attachment://att_ab12cd34') or its exact file name, as listed by read_file on attachment://"
    },
    "issue_id": {
      "type": "integer",
      "description": "Optional Redmine issue (bug) ID. When provided, the uploaded token is immediately linked to that issue with a PUT /issues/{id}.json uploads entry. When omitted, only the token is returned and you must link it yourself."
    },
    "service": {
      "type": "string",
      "description": "Optional Redmine MCP service name. Required only when the workspace has more than one Redmine MCP service; otherwise the sole service is used."
    },
    "description": {
      "type": "string",
      "description": "Optional attachment description stored by Redmine."
    }
  },
  "required": ["attachment"]
}`)
}

// redmineUploadAttachmentInput is the parsed tool argument.
type redmineUploadAttachmentInput struct {
	Attachment  string `json:"attachment"`
	IssueID     int64  `json:"issue_id"`
	Service     string `json:"service"`
	Description string `json:"description"`
}

// redmineUploadResponse mirrors the YAML/JSON envelope returned by the
// mcp-redmine tools (format_response: status_code/body/error). yaml.v3 also
// parses plain JSON, so one struct covers REDMINE_RESPONSE_FORMAT either way.
type redmineUploadResponse struct {
	StatusCode int    `yaml:"status_code" json:"status_code"`
	Error      string `yaml:"error" json:"error"`
	Body       struct {
		Upload struct {
			Token       string `yaml:"token" json:"token"`
			Filename    string `yaml:"filename" json:"filename"`
			ContentType string `yaml:"content_type" json:"content_type"`
		} `yaml:"upload" json:"upload"`
	} `yaml:"body" json:"body"`
}

type redmineRequestResponse struct {
	StatusCode int    `yaml:"status_code" json:"status_code"`
	Error      string `yaml:"error" json:"error"`
}

func (t *RedmineAttachmentUploadTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input redmineUploadAttachmentInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("failed to parse arguments: %v", err)}, nil
	}
	ref := strings.TrimSpace(input.Attachment)
	if ref == "" {
		return &types.ToolResult{Success: false, Error: "attachment is required: pass the attachment:// handle or file name shown by read_file"}, nil
	}

	svc, err := t.pickService(input.Service)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil
	}

	// 1. Authorized read: the application reader matches the ref against this
	// session's own inventory before touching storage. wait=false because the
	// original bytes exist as soon as the document row exists; text parsing is
	// irrelevant to a binary upload.
	resolved, err := t.reader.Resolve(ctx, ref, false)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil
	}
	doc := resolved.Document
	if doc == nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf(
			"attachment %q is not stored anymore; ask the user to upload it again", ref)}, nil
	}
	if doc.FileSize > t.maxBytes {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf(
			"attachment %q is %d bytes, above the Redmine upload limit of %d bytes",
			doc.FileName, doc.FileSize, t.maxBytes)}, nil
	}

	// 2. Read the raw object bytes through tenant-scoped storage, bounded by
	// the limit even if the stored size metadata is stale/missing.
	file, err := t.files.GetFile(ctx, doc.ResourceRef)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("failed to open attachment %q: %v", doc.FileName, err)}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(file, t.maxBytes+1))
	_ = file.Close()
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("failed to read attachment %q: %v", doc.FileName, err)}, nil
	}
	if int64(len(raw)) > t.maxBytes {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf(
			"attachment %q exceeds the Redmine upload limit of %d bytes", doc.FileName, t.maxBytes)}, nil
	}
	if len(raw) == 0 {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("attachment %q is empty", doc.FileName)}, nil
	}

	mimeType := resolveAttachmentMIME(doc)
	encoded := base64.StdEncoding.EncodeToString(raw)

	// 3. Call the managed mcp-redmine container. Log metadata only — never the
	// base64 payload or decoded bytes.
	logger.GetLogger(ctx).Infof(
		"redmine_upload_attachment: file=%q size=%d mime=%s service=%q issue_id=%d",
		doc.FileName, len(raw), mimeType, svc.Name, input.IssueID)

	client, err := t.caller.GetOrCreateClient(ctx, svc)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("connect Redmine MCP service %q: %v", svc.Name, err)}, nil
	}
	uploadArgs := map[string]interface{}{
		"filename":     doc.FileName,
		"content":      encoded,
		"content_type": mimeType,
	}
	if desc := strings.TrimSpace(input.Description); desc != "" {
		uploadArgs["description"] = desc
	}
	uploadResult, err := client.CallTool(ctx, redmineUploadContentTool, uploadArgs)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("redmine_upload_content call failed: %v", err)}, nil
	}
	if uploadResult == nil {
		return &types.ToolResult{Success: false, Error: "redmine_upload_content returned no result"}, nil
	}
	if uploadResult.IsError {
		return &types.ToolResult{Success: false, Error: "redmine_upload_content refused the upload: " + extractContentText(uploadResult.Content)}, nil
	}

	var upload redmineUploadResponse
	if err := yaml.Unmarshal([]byte(extractContentText(uploadResult.Content)), &upload); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("failed to parse upload response: %v", err)}, nil
	}
	if upload.StatusCode < 200 || upload.StatusCode >= 300 || upload.Body.Upload.Token == "" {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf(
			"Redmine rejected the upload (status %d): %s", upload.StatusCode, upload.Error)}, nil
	}
	token := upload.Body.Upload.Token

	data := map[string]interface{}{
		"token":        token,
		"filename":     doc.FileName,
		"content_type": mimeType,
		"size":         len(raw),
	}

	// 4. Optional second phase: a Redmine upload token is inert until an issue
	// update references it. When issue_id is given, perform that linkage here
	// so the attachment cannot end up orphaned by a forgotten follow-up call.
	if input.IssueID > 0 {
		linkArgs := map[string]interface{}{
			"path":   fmt.Sprintf("issues/%d.json", input.IssueID),
			"method": "put",
			"data": map[string]interface{}{
				"issue": map[string]interface{}{
					"uploads": []map[string]interface{}{{
						"token":        token,
						"filename":     doc.FileName,
						"content_type": mimeType,
					}},
				},
			},
		}
		linkResult, err := client.CallTool(ctx, redmineRequestTool, linkArgs)
		if err != nil {
			return &types.ToolResult{Success: false, Data: data, Error: fmt.Sprintf(
				"upload succeeded with token %q but linking it to issue %d failed: %v",
				token, input.IssueID, err)}, nil
		}
		if linkResult == nil || linkResult.IsError {
			detail := "no result"
			if linkResult != nil {
				detail = extractContentText(linkResult.Content)
			}
			return &types.ToolResult{Success: false, Data: data, Error: fmt.Sprintf(
				"upload succeeded with token %q but Redmine refused to link it to issue %d: %s",
				token, input.IssueID, detail)}, nil
		}
		var link redmineRequestResponse
		if err := yaml.Unmarshal([]byte(extractContentText(linkResult.Content)), &link); err != nil {
			return &types.ToolResult{Success: false, Data: data, Error: fmt.Sprintf(
				"failed to parse the issue %d update response: %v", input.IssueID, err)}, nil
		}
		if link.StatusCode < 200 || link.StatusCode >= 300 {
			return &types.ToolResult{Success: false, Data: data, Error: fmt.Sprintf(
				"upload token %q was created but linking issue %d failed (status %d): %s",
				token, input.IssueID, link.StatusCode, link.Error)}, nil
		}
		data["issue_id"] = input.IssueID
		return &types.ToolResult{
			Success: true,
			Data:    data,
			Output: fmt.Sprintf("Uploaded %q (%d bytes, %s) to Redmine and linked it to issue #%d. Token %s is now attached to the bug.",
				doc.FileName, len(raw), mimeType, input.IssueID, token),
		}, nil
	}

	return &types.ToolResult{
		Success: true,
		Data:    data,
		Output: fmt.Sprintf(
			"Uploaded %q (%d bytes, %s) to Redmine. The token %s is not linked to any issue yet. "+
				"To attach it to bug {id}, call the Redmine MCP redmine_request tool with method=put, "+
				`path="issues/{id}.json", data={"issue": {"uploads": [{"token": %q, "filename": %q, "content_type": %q}]}}.`,
			doc.FileName, len(raw), mimeType, token, token, doc.FileName, mimeType),
	}, nil
}

// pickService resolves which Redmine MCP service receives the upload.
func (t *RedmineAttachmentUploadTool) pickService(name string) (*types.MCPService, error) {
	if len(t.services) == 0 {
		return nil, fmt.Errorf("no Redmine MCP service is enabled in this workspace")
	}
	want := strings.TrimSpace(name)
	if want != "" {
		for _, svc := range t.services {
			if svc != nil && svc.Name == want {
				return svc, nil
			}
		}
		return nil, fmt.Errorf("Redmine MCP service %q not found; available: %s",
			name, redmineServiceNames(t.services))
	}
	if len(t.services) > 1 {
		return nil, fmt.Errorf("multiple Redmine MCP services are configured (%s); pass the service argument to choose one",
			redmineServiceNames(t.services))
	}
	return t.services[0], nil
}

func redmineServiceNames(services []*types.MCPService) string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		if svc != nil {
			names = append(names, svc.Name)
		}
	}
	return strings.Join(names, ", ")
}

// resolveAttachmentMIME prefers the MIME captured at upload time, then the OS
// MIME database by extension, then Redmine's generic binary type.
func resolveAttachmentMIME(doc *types.TemporaryDocument) string {
	if mt := strings.TrimSpace(doc.MimeType); mt != "" {
		return mt
	}
	if mt := strings.TrimSpace(mime.TypeByExtension(strings.TrimSpace(doc.FileType))); mt != "" {
		return mt
	}
	return "application/octet-stream"
}
