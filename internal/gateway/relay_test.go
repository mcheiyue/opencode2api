package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/warp"
)

func TestRelayPinBypassesWARPAndTierFallback(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer public" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if status != http.StatusOK {
					w.WriteHeader(status)
					if status == http.StatusBadRequest {
						_, _ = io.WriteString(w, `{"error":{"message":"Referenced reasoning item 'rs_expired' was not found"}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"selected relay\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			cfg := config.Config{
				ServerKeys: []string{"local-test"}, Anonymous: true,
				Proxies:  []string{"direct"},
				Upstream: config.UpstreamConfig{Anonymous: upstream.URL, Zen: upstream.URL},
				Retry:    config.RetryConfig{MaxAttempts: 3, TimeoutSeconds: 3},
			}
			if status == http.StatusTooManyRequests {
				cfg.ZenKeys = []string{"zen-test"}
			}
			gw, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			protocol := wire.Chat
			body := `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hello"}]}`
			if status == http.StatusBadRequest {
				protocol = wire.Responses
				body = `{"model":"deepseek-v4.1-flash-free","input":"hello","previous_response_id":"resp_expired"}`
			}
			gw.catalog.ReplaceWithCapabilities([]string{"deepseek-v4.1-flash-free"}, nil,
				map[config.Tier]map[string]wire.Protocol{config.TierZen: {"deepseek-v4.1-flash-free": protocol}}, nil, nil)
			// 路由哨兵：打开真实额度闸门，错误进入 WARP 时应返回 429 而不创建隧道。
			gw.warp = &warp.Pool{}
			gw.quota.open(time.Hour)
			req := httptest.NewRequest(http.MethodPost, wire.Path(protocol), strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-test")
			req = req.WithContext(WithRelayOverride(req.Context(), RelaySelection{Name: "direct"}))
			result := httptest.NewRecorder()
			gw.Handler().ServeHTTP(result, req)
			if result.Code != status {
				t.Fatalf("HTTP %d, want %d: %s", result.Code, status, result.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("pinned relay received %d requests, want exactly 1", calls.Load())
			}
			if status == http.StatusOK && !strings.Contains(result.Body.String(), "selected relay") {
				t.Fatalf("selected relay response missing: %s", result.Body.String())
			}
		})
	}
}
