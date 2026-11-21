package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/warp"
)

// freeTierCapture is one anonymous upstream request observed by the fake server.
type freeTierCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *freeTierCapture) add(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, string(body))
}

func (c *freeTierCapture) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("anonymous upstream received no request")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(c.bodies[len(c.bodies)-1]), &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	return payload
}

// freeTierHarness routes one shaped anonymous request at a capturing upstream.
func freeTierHarness(t *testing.T, environment *config.PromptEnvironmentConfig) (*Gateway, *freeTierCapture) {
	t.Helper()
	capture := &freeTierCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		capture.add(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Config{
		ServerKeys: []string{"local-test"},
		Anonymous:  true,
		Proxies:    []string{"direct"},
		Upstream: config.UpstreamConfig{
			Anonymous:         upstream.URL,
			Zen:               upstream.URL,
			PromptEnvironment: environment,
		},
		Retry: config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 3},
	}
	gw, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gw.catalog.ReplaceWithCapabilities([]string{"deepseek-v4.1-flash-free"}, nil,
		map[config.Tier]map[string]wire.Protocol{config.TierZen: {"deepseek-v4.1-flash-free": wire.Chat}}, nil, nil)
	gw.warp = &warp.Pool{}
	return gw, capture
}

func postFreeTier(t *testing.T, gw *Gateway, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, wire.Path(wire.Chat), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local-test")
	req = req.WithContext(WithRelayOverride(req.Context(), RelaySelection{Name: "direct"}))
	result := httptest.NewRecorder()
	gw.Handler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", result.Code, result.Body.String())
	}
	return result
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %q: %v", value, err)
	}
	return string(encoded)
}

func systemPromptOf(t *testing.T, payload map[string]any) string {
	t.Helper()
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("upstream payload has no messages: %v", payload)
	}
	first, _ := messages[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("first upstream message role is %v, want system", first["role"])
	}
	content, _ := first["content"].(string)
	if content == "" {
		t.Fatalf("system message carries no text")
	}
	return content
}

// The gateway host must never describe itself: without any client declaration
// and without configured defaults, the environment block stays unknown.
func TestFreeTierPromptKeepsHostEnvironmentUnknown(t *testing.T) {
	gw, capture := freeTierHarness(t, &config.PromptEnvironmentConfig{})
	postFreeTier(t, gw, `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hello"}]}`)
	prompt := systemPromptOf(t, capture.last(t))
	for _, line := range []string{
		"Working directory: unknown",
		"Workspace root folder: unknown",
		"Platform: unknown",
	} {
		if !strings.Contains(prompt, line) {
			t.Fatalf("prompt is missing %q:\n%s", line, prompt)
		}
	}
}

// A client that declares its own <env> block owns every environment field: the
// deployment default must not leak into a request that described itself.
func TestFreeTierPromptPrefersNativeClientEnvironment(t *testing.T) {
	configured := &config.PromptEnvironmentConfig{
		WorkingDirectory: "/srv/gateway-default",
		WorkspaceRoot:    "/srv/gateway-default",
		Platform:         "linux",
	}
	gw, capture := freeTierHarness(t, configured)
	system := "You are a coding agent.\n<env>\n  Working directory: /home/dev/app\n  Is directory a git repo: yes\n  Platform: darwin\n</env>"
	body := `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"system","content":` + jsonString(t, system) + `},{"role":"user","content":"hello"}]}`
	postFreeTier(t, gw, body)
	prompt := systemPromptOf(t, capture.last(t))
	for _, line := range []string{"Working directory: /home/dev/app", "Platform: darwin", "Is directory a git repo: yes"} {
		if !strings.Contains(prompt, line) {
			t.Fatalf("prompt is missing client value %q:\n%s", line, prompt)
		}
	}
	if strings.Contains(prompt, "/srv/gateway-default") {
		t.Fatalf("deployment default leaked into a self-described request:\n%s", prompt)
	}
	if strings.Contains(prompt, "Platform: linux") {
		t.Fatalf("configured platform leaked into a self-described request:\n%s", prompt)
	}
}

// The gateway-only extension must be consumed and never forwarded upstream.
func TestFreeTierPromptConsumesClientEnvironmentExtension(t *testing.T) {
	configured := &config.PromptEnvironmentConfig{WorkingDirectory: "/srv/gateway-default", Platform: "linux"}
	gw, capture := freeTierHarness(t, configured)
	body := `{"model":"deepseek-v4.1-flash-free","client_environment":{"working_directory":"C:\\work\\repo","workspace_root":"C:\\work","platform":"win32","is_git_repo":true},"messages":[{"role":"user","content":"hello"}]}`
	postFreeTier(t, gw, body)
	payload := capture.last(t)
	if _, forwarded := payload["client_environment"]; forwarded {
		t.Fatalf("client_environment reached the upstream body: %v", payload)
	}
	prompt := systemPromptOf(t, payload)
	for _, line := range []string{`Working directory: C:\work\repo`, `Workspace root folder: C:\work`, "Platform: win32", "Is directory a git repo: yes"} {
		if !strings.Contains(prompt, line) {
			t.Fatalf("prompt is missing explicit client value %q:\n%s", line, prompt)
		}
	}
	if strings.Contains(prompt, "/srv/gateway-default") {
		t.Fatalf("deployment default leaked past an explicit client snapshot:\n%s", prompt)
	}
}

// A malformed native declaration is an unknown client, not a licence to fall
// back to the deployment default.
func TestFreeTierPromptRejectsMalformedNativeEnvironment(t *testing.T) {
	configured := &config.PromptEnvironmentConfig{WorkingDirectory: "/srv/gateway-default", Platform: "linux"}
	gw, capture := freeTierHarness(t, configured)
	system := "You are a coding agent.\n<env>\n  Working directory: /home/dev/app\n"
	body := `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"system","content":` + jsonString(t, system) + `},{"role":"user","content":"hello"}]}`
	postFreeTier(t, gw, body)
	prompt := systemPromptOf(t, capture.last(t))
	if strings.Contains(prompt, "/srv/gateway-default") {
		t.Fatalf("deployment default applied after a malformed declaration:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Working directory: unknown") {
		t.Fatalf("malformed declaration should leave the environment unknown:\n%s", prompt)
	}
}

// Probes and other internal callers have no ingress snapshot and must receive
// the deployment configuration, never the gateway host environment.
func TestRequestEnvironmentWithoutSnapshotUsesConfiguration(t *testing.T) {
	configured := &config.PromptEnvironmentConfig{WorkingDirectory: "/srv/gateway-default", Platform: "linux"}
	gw, _ := freeTierHarness(t, configured)
	environment := gw.requestEnvironment(context.Background())
	if environment == nil {
		t.Fatal("requestEnvironment returned nil")
	}
	if environment.WorkingDirectory != "/srv/gateway-default" || environment.Platform != "linux" {
		t.Fatalf("probe environment = %+v, want the configured values", *environment)
	}
}
