package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

// toolRead is the clean-core source read entry point. Environment facts use
// environment_read so each semantic operation has one canonical public tool.
func (r *Runtime) toolRead(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, view := canonicalReadRequest(req)
	if err := validateReadViewArguments(req, view); err != nil {
		envReq, _, fail := r.remoteRequest(ctx, req)
		if fail != nil {
			return fail, nil
		}
		return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "INVALID_ARGUMENT", err.Error())
	}
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

func validateReadViewArguments(req *mcp.CallToolRequest, view string) error {
	args := mcpresult.Arguments(req)
	path, _ := args["path"].(string)
	path = strings.TrimSpace(path)
	paths, _ := args["paths"].([]any)
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	switch view {
	case "search":
		if path != "" {
			return fmt.Errorf("read(view=search) does not accept path; provide paths[] to keep the search scope explicit")
		}
		if len(paths) == 0 {
			return fmt.Errorf("read(view=search) requires non-empty paths[]")
		}
	case "context":
		if path != "" {
			return fmt.Errorf("read(view=context) does not accept path; provide paths[] when a scope is required")
		}
	case "file":
		if query != "" || len(paths) > 0 {
			return fmt.Errorf("read(view=file) accepts path/items, not query or paths[]")
		}
	case "list":
		if query != "" || len(paths) > 0 {
			return fmt.Errorf("read(view=list) accepts path as its scope, not query or paths[]")
		}
	}
	return nil
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
