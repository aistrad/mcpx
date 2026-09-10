package server

import "mcpx/internal/approval"

// pendingConfirmationItems builds the session bootstrap view for all operations
// that still need an explicit user confirmation.
func pendingConfirmationItems(pending []approval.Pending) []map[string]any {
	items := make([]map[string]any, 0, len(pending))
	for _, pendingItem := range pending {
		tool := pendingItem.Tool
		if tool == "command_execute" {
			tool = "execute"
		}
		item := map[string]any{
			"tool":       tool,
			"summary":    pendingItem.Summary,
			"workspace":  pendingItem.Workspace,
			"created_at": pendingItem.CreatedAt,
		}
		if pendingItem.Tool == "mcp_tool" && pendingItem.ConfirmationToken != "" {
			item["confirmation_key"] = pendingItem.ConfirmationToken
		} else {
			item["user_confirmed_required"] = true
		}
		if pendingItem.Command != "" {
			item["command"] = pendingItem.Command
			item["purpose"] = pendingItem.Purpose
			item["scope"] = pendingItem.Scope
		}
		items = append(items, item)
	}
	return items
}
