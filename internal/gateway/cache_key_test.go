package gateway

import (
	"encoding/json"
	"testing"

	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

func TestV2ChatBodyStatesSessionAsCacheKey(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	session := "ses_0123456789abFFEdcba9876543210"

	shaped, _ := prepareAnonymousBody(body, wire.Chat, false, session)

	var payload map[string]any
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if got := payload["prompt_cache_key"]; got != session {
		t.Fatalf("prompt_cache_key = %v, want %q", got, session)
	}
}

func TestV2ChatBodyKeepsCallerCacheKey(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","prompt_cache_key":"caller-key","messages":[{"role":"user","content":"hi"}]}`)

	shaped, _ := prepareAnonymousBody(body, wire.Chat, false, "ses_0123456789abFFEdcba9876543210")

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

	shaped, _ := prepareAnonymousBody(body, wire.Chat, true, "ses_0123456789abFFEdcba9876543210")

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
