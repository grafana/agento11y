package hook

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

func TestSessionStart_PreservesConversationTitle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(&bytes.Buffer{}, "", 0)

	if err := fragment.SaveSession(fragment.Session{
		ConversationID:    "conv",
		ConversationTitle: "list go files",
		StartedAt:         "2026-04-28T12:00:00Z",
	}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	SessionStart(Payload{
		HookEventName:  "sessionStart",
		ConversationID: "conv",
		WorkspaceRoots: []string{"/repo"},
		UserEmail:      "dev@example.com",
		CursorVersion:  "2.0",
		Timestamp:      "2026-04-28T12:01:00Z",
	}, logger)

	got := fragment.LoadSession("conv", logger)
	if got == nil {
		t.Fatal("expected session")
	}
	if got.ConversationTitle != "list go files" {
		t.Errorf("ConversationTitle = %q; want list go files", got.ConversationTitle)
	}
	if got.UserEmail != "dev@example.com" {
		t.Errorf("UserEmail = %q; want the sessionStart payload", got.UserEmail)
	}
	if len(got.WorkspaceRoots) != 1 || got.WorkspaceRoots[0] != "/repo" {
		t.Errorf("WorkspaceRoots = %v; want [/repo]", got.WorkspaceRoots)
	}
}
