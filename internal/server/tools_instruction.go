package server

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/instruction"
)

func (r *Runtime) toolAgentInstructionList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, remote, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	anchor, _ := envReq.Payload["anchor_path"].(string)
	var paths []string
	if raw, ok := envReq.Payload["paths"].([]any); ok {
		for _, item := range raw {
			if p, ok := item.(string); ok && p != "" {
				paths = append(paths, p)
			}
		}
	}
	maxBytes := r.effectiveConfig(remote.WorkspacePath).Security.Files.MaxReadBytes
	docs := instruction.DiscoverAt(
		r.cfg.Discovery.Instructions.GlobalAgentsPath, remote.WorkspacePath, anchor, maxBytes,
	)
	data := map[string]any{
		"instructions":         docs,
		"anchor_path":          anchor,
		"instruction_revision": instructionRevision(docs),
	}
	if len(paths) > 0 {
		data["resolution"] = instruction.ResolveForPaths(
			r.cfg.Discovery.Instructions.GlobalAgentsPath, remote.WorkspacePath, paths, maxBytes,
		)
	}
	return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, data)
}

func (r *Runtime) toolAgentInstructionRead(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, remote, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	id, _ := envReq.Payload["id"].(string)
	anchor, _ := envReq.Payload["anchor_path"].(string)
	document, content, err := instruction.ReadAt(
		r.cfg.Discovery.Instructions.GlobalAgentsPath,
		remote.WorkspacePath,
		anchor,
		id,
		r.effectiveConfig(remote.WorkspacePath).Security.Files.MaxReadBytes,
	)
	if err != nil {
		code := "instruction_read_error"
		if errors.Is(err, instruction.ErrNotFound) {
			code = "instruction_not_found"
		}
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, nil, code, err.Error())
		response.RemoteSessionID = remote.ID
		return r.resultJSON(response)
	}
	data := map[string]any{"instruction": document, "content": content}
	return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, data)
}

func (r *Runtime) agentInstructions(workspacePath string) []instruction.Document {
	return instruction.Discover(
		r.cfg.Discovery.Instructions.GlobalAgentsPath,
		workspacePath,
		r.effectiveConfig(workspacePath).Security.Files.MaxReadBytes,
	)
}
