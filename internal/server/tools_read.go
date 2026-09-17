package server

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

// toolRead is the clean-core source read entry point. Environment facts use
// environment_read so each semantic operation has one canonical public tool.
func (r *Runtime) toolRead(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, view := canonicalReadRequest(req)
	switch view {
	case "file", "search", "list", "context":
		return r.toolSourceRead(ctx, req)
	default:
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "ambiguous_request", "read view cannot be inferred uniquely; provide view when the arguments are ambiguous")
	}
}

// canonicalReadRequest keeps the public read surface forgiving while the
// internal handlers continue to receive an explicit, strict view. Only views
// that are uniquely implied by the supplied arguments are inferred.
func canonicalReadRequest(req *mcp.CallToolRequest) (*mcp.CallToolRequest, string) {
	if view := publicSelector(req, "view"); view != "" {
		return req, view
	}
	args := mcpresult.Arguments(req)
	view := inferReadView(args)
	if view == "" {
		return req, ""
	}
	return forwardedRequest(req, map[string]any{"view": view}), view
}

func inferReadView(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	candidates := map[string]bool{}
	if items, ok := args["items"].([]any); ok && len(items) > 0 {
		candidates["file"] = true
	}
	if hasReadArgument(args, "mode", "offset", "max_total_bytes", "max_bytes_per_file") {
		candidates["file"] = true
	}
	if hasReadArgument(args, "entries_cursor", "entries_limit") {
		candidates["list"] = true
	}
	if query, _ := args["query"].(string); strings.TrimSpace(query) != "" {
		if mode, _ := args["search_mode"].(string); strings.TrimSpace(mode) != "" {
			candidates["context"] = true
		} else {
			candidates["search"] = true
		}
	}
	if len(candidates) != 1 {
		return ""
	}
	for view := range candidates {
		return view
	}
	return ""
}

func hasReadArgument(args map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := args[key]; ok && value != nil {
			return true
		}
	}
	return false
}
