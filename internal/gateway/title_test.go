package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestIsTitleGenerationRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"legacy", `{"messages":[{"role":"user","content":"Generate a title for this conversation:\nUser: Fix login bug"}]}`, true},
		{"v2", `{"messages":[{"role":"system","content":"You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else."},{"role":"user","content":"Implement auth middleware"}]}`, true},
		{"v2 for it", `{"messages":[{"role":"developer","content":"Reply with a title for it and nothing else."},{"role":"user","content":"Fix typo"}]}`, true},
		{"rename", `{"messages":[{"role":"system","content":"You decide whether the session title still matches the conversation."},{"role":"user","content":"Database migration"}]}`, true},
		{"declared tools", `{"messages":[{"role":"system","content":"Reply with a title and nothing else."}],"tools":[{"type":"function","function":{"name":"shell"}}]}`, false},
		{"legacy functions", `{"messages":[{"role":"system","content":"Reply with a title and nothing else."}],"functions":[{"name":"shell"}]}`, false},
		{"ordinary question", `{"messages":[{"role":"user","content":"What is the capital of France?"}]}`, false},
		{"quoted title prompt", `{"messages":[{"role":"user","content":"Review this prompt: You name conversations so the user can find them later in a list of past sessions."}]}`, false},
		{"legacy substring", `{"messages":[{"role":"user","content":"How does OpenCode generate a title for this conversation?"}]}`, false},
		{"conversation", `{"messages":[{"role":"system","content":"Reply with a title and nothing else."},{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTitleGenerationBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("title recognition = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrepareAnonymousBodyTitleTools(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		v1       bool
		toolFree bool
	}{
		{"v2 title", `{"messages":[{"role":"system","content":"Reply with a title and nothing else."},{"role":"user","content":"Add Redis caching"}]}`, false, true},
		{"v1 title", `{"messages":[{"role":"user","content":"Generate a title for this conversation:\nAdd Redis caching"}]}`, true, true},
		{"agent", `{"messages":[{"role":"user","content":"Explain Go channels"}]}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := prepareAnonymousBody([]byte(tc.body), wire.Chat, tc.v1, "ses_title")
			var payload struct {
				Tools []json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if (len(payload.Tools) == 0) != tc.toolFree {
				t.Fatalf("tool count = %d, want tool-free = %v", len(payload.Tools), tc.toolFree)
			}
		})
	}
}

func TestSynthesizeTitleUnicodeAndFallbacks(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"Fix authentication bug in middleware", "Fix authentication bug in middleware"},
		{"Türkçe başlık oluşturma ve kota sınırlaması çözümü", "Türkçe başlık oluşturma ve kota sınırlam"},
		{"Generate a title for this conversation:\nUser: Upgrade database schema to v3", "User: Upgrade database schema to v3"},
		{"   ", "New Session"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			payload := map[string]any{"messages": []any{map[string]any{"role": "user", "content": tc.text}}}
			if got := synthesizeTitle(payload); got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
		})
	}
	if got := synthesizeTitle(map[string]any{"messages": []any{map[string]any{"role": "system", "content": "System prompt only"}}}); got != "New Session" {
		t.Fatalf("title without user = %q", got)
	}
}

func TestTitleFallbackThroughHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
			}))
			defer upstream.Close()
			cfg := config.Config{
				ServerKeys: []string{"local-test"}, Anonymous: true,
				Upstream: config.UpstreamConfig{Anonymous: upstream.URL},
				Retry:    config.RetryConfig{MaxAttempts: 12, TimeoutSeconds: 3},
			}
			gw, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			gw.catalog.ReplaceWithCapabilities([]string{"deepseek-v4.1-flash-free"}, nil,
				map[config.Tier]map[string]wire.Protocol{config.TierZen: {"deepseek-v4.1-flash-free": wire.Chat}}, nil, nil)
			body, err := json.Marshal(map[string]any{
				"model": "deepseek-v4.1-flash-free", "stream": stream,
				"messages": []any{
					map[string]any{"role": "system", "content": "Reply with a title and nothing else."},
					map[string]any{"role": "user", "content": "Refactor auth middleware"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer local-test")
			result := httptest.NewRecorder()
			gw.Handler().ServeHTTP(result, req)
			if result.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", result.Code, result.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("title made %d upstream attempts, want 1", calls.Load())
			}
			response := result.Body.Bytes()
			if stream {
				response, err = wire.CollapseStream(strings.NewReader(string(response)), wire.Chat, "deepseek-v4.1-flash-free")
				if err != nil {
					t.Fatal(err)
				}
			}
			var completion struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(response, &completion); err != nil {
				t.Fatal(err)
			}
			if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "Refactor auth middleware" {
				t.Fatalf("unexpected fallback: %s", response)
			}
		})
	}
}
