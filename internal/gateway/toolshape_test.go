package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

func TestPrepareAnonymousBodyHidesClientToolNames(t *testing.T) {
	const session = "ses_0123456789abFFEdcba9876543210"
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"ReportFindings","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"ScheduleWakeup","parameters":{"type":"object","properties":{}}}}
	]}`)

	shaped, renames := prepareAnonymousBody(body, wire.Chat, false, session)
	if len(shaped) == 0 {
		t.Fatal("no shaped body")
	}
	text := string(shaped)
	for _, marker := range []string{"ReportFindings", "ScheduleWakeup"} {
		if strings.Contains(text, marker) {
			t.Errorf("shaped body still spells client tool %q", marker)
		}
	}

	var payload struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		Tools          []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if payload.PromptCacheKey != session {
		t.Errorf("prompt_cache_key = %q, want session %q", payload.PromptCacheKey, session)
	}
	names := make(map[string]bool, len(payload.Tools))
	for _, tool := range payload.Tools {
		names[tool.Function.Name] = true
	}
	for _, official := range []string{"read", "edit", "shell"} {
		if !names[official] {
			t.Errorf("shaped body is missing official tool %q", official)
		}
	}
	for _, client := range []string{"Read", "ReportFindings", "ScheduleWakeup"} {
		wireName, ok := renames.Wire(client)
		if !ok || !names[wireName] {
			t.Errorf("client tool %q is not declared under an opaque wire name", client)
		}
	}
}

func TestPrepareAnonymousBodyRestoresToolCalls(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"ScheduleWakeup","parameters":{"type":"object","properties":{}}}}]}`)
	_, renames := prepareAnonymousBody(body, wire.Chat, false, "ses_0123456789abFFEdcba9876543210")
	wireName, ok := renames.Wire("ScheduleWakeup")
	if !ok {
		t.Fatal("no wire name recorded")
	}
	if got := renames.Restore(wireName); got != "ScheduleWakeup" {
		t.Errorf("Restore(%q) = %q, want ScheduleWakeup", wireName, got)
	}
}
