package gateway

import (
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

// A rotation alias whose group model routes to the anonymous lane must stay
// advertised and servable without a Go key: routing the group through the Go
// tier would hide the alias and strand free-tier rotation.
func TestRotationAliasServedByAnonymousLane(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+anonymousZenKey {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"rotated\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := config.Config{
		ServerKeys: []string{"local-test"}, Anonymous: true,
		Proxies:  []string{"direct"},
		Upstream: config.UpstreamConfig{Anonymous: upstream.URL, Zen: upstream.URL},
		Retry:    config.RetryConfig{MaxAttempts: 3, TimeoutSeconds: 5},
		Rotation: config.RotationConfig{Sweet: config.RotationGroup{Alias: "sweet", Order: []string{"qa-free-model"}}},
	}
	gw, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gw.catalog.ReplaceWithCapabilities([]string{"qa-free-model"}, nil,
		map[config.Tier]map[string]wire.Protocol{config.TierZen: {"qa-free-model": wire.Chat}}, nil, nil)
	gw.syncRotation()

	list := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer local-test")
	gw.Handler().ServeHTTP(list, request)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"sweet"`) {
		t.Fatalf("rotation alias missing from the model list: %d %s", list.Code, list.Body.String())
	}

	body := `{"model":"sweet","messages":[{"role":"user","content":"hello"}]}`
	result := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer local-test")
	gw.Handler().ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("HTTP %d, want 200: %s", result.Code, result.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("anonymous upstream received %d requests, want exactly 1", calls.Load())
	}
	if !strings.Contains(result.Body.String(), `"model":"sweet"`) {
		t.Fatalf("reply did not name the group alias: %s", result.Body.String())
	}
}
