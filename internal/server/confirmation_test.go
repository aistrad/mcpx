package server

import (
	"testing"
	"time"

	"mcpx/internal/approval"
)

func TestPendingConfirmationItemsPreservesExtensionToolName(t *testing.T) {
	items := pendingConfirmationItems([]approval.Pending{
		{Tool: "command_execute", Summary: "printf ok", CreatedAt: time.Unix(1, 0)},
		{Tool: "skill_tool", Summary: "publish", CreatedAt: time.Unix(2, 0)},
		{Tool: "mcp_tool", Summary: "dbx/query", ConfirmationToken: "ct_dbx", CreatedAt: time.Unix(3, 0)},
	})
	if len(items) != 3 {
		t.Fatalf("pending confirmation items=%+v", items)
	}
	for index, want := range []string{"execute", "skill_tool", "mcp_tool"} {
		if items[index]["tool"] != want {
			t.Fatalf("pending confirmation item %d tool=%q want=%q", index, items[index]["tool"], want)
		}
	}
	if items[0]["user_confirmed_required"] != true || items[1]["user_confirmed_required"] != true {
		t.Fatalf("boolean confirmations changed unexpectedly: %+v", items)
	}
	if items[2]["confirmation_key"] != "ct_dbx" || items[2]["user_confirmed_required"] != nil {
		t.Fatalf("mcp_tool pending confirmation must expose its key: %+v", items[2])
	}
}

func TestExtensionConfirmationContentKeyIgnoresPurposeButBindsEffect(t *testing.T) {
	for _, operation := range []string{"mcp_tool", "skill_tool"} {
		t.Run(operation, func(t *testing.T) {
			base := map[string]any{
				"purpose": "执行已确认的数据库变更",
				"server":  "dbx",
				"tool":    "dbx_execute_query",
				"arguments": map[string]any{
					"sql": "SELECT 1",
				},
			}
			rephrased := map[string]any{
				"purpose": "按用户语义要求完成相同操作",
				"server":  "dbx",
				"tool":    "dbx_execute_query",
				"arguments": map[string]any{
					"sql": "SELECT 1",
				},
			}
			if got, want := extensionConfirmationContentKey("principal", operation, "dbx/dbx_execute_query", "revision-1", base), extensionConfirmationContentKey("principal", operation, "dbx/dbx_execute_query", "revision-1", rephrased); got != want {
				t.Fatalf("purpose-only change altered confirmation key: got=%q want=%q", got, want)
			}

			changedEffect := map[string]any{
				"purpose": "按用户语义要求完成相同操作",
				"server":  "dbx",
				"tool":    "dbx_execute_query",
				"arguments": map[string]any{
					"sql": "SELECT 2",
				},
			}
			if got, want := extensionConfirmationContentKey("principal", operation, "dbx/dbx_execute_query", "revision-1", base), extensionConfirmationContentKey("principal", operation, "dbx/dbx_execute_query", "revision-1", changedEffect); got == want {
				t.Fatalf("effect change reused confirmation key: %q", got)
			}
		})
	}
}
