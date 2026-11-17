package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/agent/policy"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type staticPolicyGate struct {
	enabled bool
	err     error
}

func (g staticPolicyGate) Action(context.Context, uint64, string, *types.MCPTool) policy.Action {
	return policy.Action{Risk: policy.RiskRead}
}

func (g staticPolicyGate) IsEnabled(context.Context, uint64, string, string) (bool, error) {
	return g.enabled, g.err
}

func (g staticPolicyGate) RequestAndWait(context.Context, approval.PendingRequest) (approval.Decision, error) {
	return approval.Decision{Approved: true}, nil
}

func (staticPolicyGate) ResumeDecision(_ context.Context, p *types.AgentPrompt, _ *event.EventBus) approval.Decision {
	return approval.Decision{PendingID: p.ID, Approved: true}
}

func (staticPolicyGate) MarkExecuted(context.Context, string) {}

var _ approval.MCPApproval = staticPolicyGate{}

func TestMCPToolExecuteDisabledByPolicy(t *testing.T) {
	tool := &MCPTool{
		service: &types.MCPService{ID: "svc-1", Name: "weather"},
		mcpTool: &types.MCPTool{Name: "get_forecast"},
		gate:    staticPolicyGate{enabled: false},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	result, err := tool.Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "MCP tool is disabled", result.Error)
}

func TestMCPToolExecutePolicyError(t *testing.T) {
	tool := &MCPTool{
		service: &types.MCPService{ID: "svc-1", Name: "weather"},
		mcpTool: &types.MCPTool{Name: "get_forecast"},
		gate:    staticPolicyGate{err: errors.New("db down")},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	result, err := tool.Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "db down")
}

func TestMCPToolExecuteMissingTenantFailClosed(t *testing.T) {
	tool := &MCPTool{
		service: &types.MCPService{ID: "svc-1", Name: "weather"},
		mcpTool: &types.MCPTool{Name: "get_forecast"},
		gate:    staticPolicyGate{enabled: true},
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "MCP tool is disabled", result.Error)
}

type redactedApprovalArgsGate struct {
	staticPolicyGate
	pending approval.PendingRequest
	marked  bool
}

func (g *redactedApprovalArgsGate) Action(context.Context, uint64, string, *types.MCPTool) policy.Action {
	return policy.Action{Risk: policy.RiskWrite, Always: true}
}

func (g *redactedApprovalArgsGate) RequestAndWait(
	_ context.Context, pending approval.PendingRequest,
) (approval.Decision, error) {
	g.pending = pending
	return approval.Decision{Approved: true, ModifiedArgs: json.RawMessage(`{"filename":"edited.txt"}`)}, nil
}

func (g *redactedApprovalArgsGate) MarkExecuted(context.Context, string) {
	g.marked = true
}

func TestMCPToolRedactedApprovalArgsHidePayloadAndRejectEdits(t *testing.T) {
	gate := &redactedApprovalArgsGate{staticPolicyGate: staticPolicyGate{enabled: true}}
	tool := &MCPTool{
		service: &types.MCPService{ID: "svc-1", Name: "Redmine"},
		mcpTool: &types.MCPTool{Name: "redmine_upload_content"},
		gate:    gate,
	}
	ctx := types.WithAccessLevel(context.Background(), types.AssistantAccessConfirm)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(7))
	ctx = WithToolExecContext(ctx, &ToolExecContext{EventBus: event.NewEventBus()})

	result, err := tool.ExecuteWithApprovalArgs(
		ctx,
		json.RawMessage(`{"filename":"screen.png","content":"secret-base64"}`),
		json.RawMessage(`{"filename":"screen.png","size_bytes":12}`),
	)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "does not support argument edits")
	require.JSONEq(t, `{"filename":"screen.png","size_bytes":12}`, string(gate.pending.Args))
	require.NotContains(t, string(gate.pending.Args), "secret-base64")
	require.True(t, gate.marked, "the resolved prompt must close even when argument edits are rejected")
}
