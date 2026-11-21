package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

func TestV2ChatBodyStatesSessionAsCacheKey(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	session := "ses_0123456789abFFEdcba9876543210"

	environment := &config.PromptEnvironmentConfig{
		WorkingDirectory: "C:/agent/work",
		WorkspaceRoot:    "C:/agent",
		Platform:         "win32",
	}
	shaped, _ := prepareAnonymousBody(body, wire.Chat, false, session, environment)

	var payload map[string]any
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if got := payload["prompt_cache_key"]; got != session {
		t.Fatalf("prompt_cache_key = %v, want %q", got, session)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %v, want injected system message", payload["messages"])
	}
	system, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("first message = %T, want object", messages[0])
	}
	content, ok := system["content"].(string)
	if !ok || !strings.Contains(content, "Working directory: C:/agent/work") || !strings.Contains(content, "Workspace root folder: C:/agent") || !strings.Contains(content, "Platform: win32") {
		t.Fatalf("injected environment does not preserve configured values: %q", content)
	}
}

func TestV2ChatBodyKeepsCallerCacheKey(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","prompt_cache_key":"caller-key","messages":[{"role":"user","content":"hi"}]}`)

	shaped, _ := prepareAnonymousBody(body, wire.Chat, false, "ses_0123456789abFFEdcba9876543210", nil)

	var payload map[string]any
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if got := payload["prompt_cache_key"]; got != "caller-key" {
		t.Fatalf("prompt_cache_key = %v, want the caller's key", got)
	}
}

func TestV1BodyCarriesNoCacheKey(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)

	shaped, _ := prepareAnonymousBody(body, wire.Chat, true, "ses_0123456789abFFEdcba9876543210", nil)

	var payload map[string]any
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if _, ok := payload["prompt_cache_key"]; ok {
		t.Fatalf("v1 body carries prompt_cache_key, which the 1.x client never sends")
	}
}

func TestV2RootSessionFollowsParent(t *testing.T) {
	parent := "ses_0123456789abFFEdcba9876543210"
	ids := identity.RequestIDs{Session: "ses_0123456789abFFEdcba9876543212", ParentSession: parent}
	want := identity.CanonicalSessionID(parent)
	if got := v2RootSession(ids); got != want {
		t.Fatalf("v2RootSession = %q, want the canonical parent %q", got, want)
	}
	if got := v2RootSession(ids); got == ids.Session {
		t.Fatalf("v2RootSession returned the child session %q", got)
	}
}
