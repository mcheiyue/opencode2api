package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"opencode2api/internal/config"
)

func formatFields(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "<unencodable>"
	}
	return string(encoded)
}

func newTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer, *LogBroker) {
	t.Helper()
	output := &bytes.Buffer{}
	hub := LogHub(100)
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	redactor := config.SecretRedactor()
	redactor.Replace(config.Config{ServerKeys: []string{"sk-configured-secret"}})
	return structuredLogger(output, level, hub, redactor), output, hub
}

func lastLogEvent(t *testing.T, hub *LogBroker) LogEvent {
	t.Helper()
	events, _ := hub.Recent(0, 1)
	if len(events) != 1 {
		t.Fatalf("recent events = %d, want 1", len(events))
	}
	return events[0]
}

// A configured secret and an unlisted credential used as a value must not
// survive in either the JSON stream or the mirrored hub event, however deeply
// they are nested: slog groups, marshalled structs, maps and slices.
func TestStructuredLoggerRedactsNestedValues(t *testing.T) {
	logger, output, hub := newTestLogger(t)
	logger.Info("upstream failed sk-configured-secret",
		"component", "upstream",
		"event", "upstream_error",
		"path", "/v1/chat/completions",
		slog.Group("request",
			"authorization", "Bearer sk-configured-secret",
			"api_token", "sk-unknown-secret",
			"url", "https://example.invalid/v1?key=sk-configured-secret",
		),
		"payload", map[string]any{
			"model": "gpt-5",
			"nested": map[string]any{
				"password":         "hunter2",
				"note":             "sk-configured-secret",
				"refreshToken":     "refresh-secret",
				"session_token":    "session-token-secret",
				"reasoning_tokens": 42,
			},
			"items": []any{map[string]any{"cookie": "session=abc123", "x-api-token": "nested-api-secret"}},
		},
		"err", errors.New("dial failed for key sk-configured-secret"),
	)

	event := lastLogEvent(t, hub)
	jsonText := output.String()
	hubText := event.Message + " " + formatFields(event.Fields)
	for _, secret := range []string{"sk-configured-secret", "sk-unknown-secret", "hunter2", "session=abc123", "refresh-secret", "session-token-secret", "nested-api-secret"} {
		if strings.Contains(jsonText, secret) {
			t.Errorf("JSON output leaked %q: %s", secret, jsonText)
		}
		if strings.Contains(hubText, secret) {
			t.Errorf("hub event leaked %q: %s", secret, hubText)
		}
	}

	if event.Component != "upstream" || event.Level != "info" {
		t.Fatalf("event schema changed: component=%q level=%q", event.Component, event.Level)
	}
	if got := event.Fields["path"]; got != "/v1/chat/completions" {
		t.Errorf("path = %v, want the routed path", got)
	}
	if got := event.Fields["request.url"]; got != "https://example.invalid/v1?key=***" {
		t.Errorf("request.url = %v, want the redacted URL", got)
	}
	if got := event.Fields["request.authorization"]; got != "***" {
		t.Errorf("request.authorization = %v, want ***", got)
	}
	payload, ok := event.Fields["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v, want the nested field object", event.Fields["payload"])
	}
	if got := payload["model"]; got != "gpt-5" {
		t.Errorf("payload.model = %v, want gpt-5", got)
	}
	nested, ok := payload["nested"].(map[string]any)
	if !ok {
		t.Fatalf("payload.nested = %v, want the nested field object", payload["nested"])
	}
	if got := nested["reasoning_tokens"]; got != json.Number("42") {
		t.Errorf("payload.nested.reasoning_tokens = %v, want 42", got)
	}
	for _, key := range []string{"refreshToken", "session_token"} {
		if got := nested[key]; got != "***" {
			t.Errorf("payload.nested.%s = %v, want ***", key, got)
		}
	}
}

// The opt-in request-body dump records the exact bytes that go upstream, so a
// string value must keep its own key names untouched: only configured secrets
// are replaced, exactly as before. Key-based masking applies to structured
// fields, not to text that is itself a payload.
func TestStructuredLoggerPreservesRequestBodyDump(t *testing.T) {
	logger, output, hub := newTestLogger(t)
	dumped := `{"model":"gpt-5","api_token":"sk-unknown-secret","nested":{"password":"hunter2"},"prompt":"hi"}`
	logger.Debug("outbound upstream request body", "component", "upstream", "event", "upstream_request_body",
		"tier", "zen", "truncated", false, "body", dumped)

	event := lastLogEvent(t, hub)
	if got := event.Fields["body"]; got != dumped {
		t.Errorf("body = %v, want the exact dumped bytes", got)
	}
	if !strings.Contains(output.String(), `"event":"upstream_request_body"`) {
		t.Errorf("JSON output lost the dump event schema: %s", output.String())
	}
}
