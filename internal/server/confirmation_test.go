package server

import (
	"testing"
	"time"

	"mcpx/internal/approval"
)

func TestPendingConfirmationItemsHideExtensionConfirmations(t *testing.T) {
	items := pendingConfirmationItems([]approval.Pending{
		{Tool: "command_execute", Summary: "printf ok", CreatedAt: time.Unix(1, 0)},
		{Tool: "skill_tool", Summary: "publish", CreatedAt: time.Unix(2, 0)},
		{Tool: "mcp_tool", Summary: "dbx/query", ConfirmationToken: "ct_dbx", CreatedAt: time.Unix(3, 0)},
	})
	if len(items) != 1 {
		t.Fatalf("extension confirmations must be hidden after server-side gates are removed: %+v", items)
	}
	if items[0]["tool"] != "execute" || items[0]["user_confirmed_required"] != true {
		t.Fatalf("non-extension confirmation changed unexpectedly: %+v", items[0])
	}
}
