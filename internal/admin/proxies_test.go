package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/telemetry"
)

const testSessionToken = "test-session-token"
const testCSRFToken = "test-csrf-token"

// proxyTestServer builds a management server over a runtime whose background
// loops are started on an already-canceled context, so no refresh or probe
// leaves the process while the handlers run. Every upstream here is loopback.
func proxyTestServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.ServerKeys = []string{"server-secret"}
	cfg.Proxies = []string{"direct", "socks5://127.0.0.1:2#pool"}
	cfg.Upstream = config.UpstreamConfig{Zen: "http://127.0.0.1:1"}
	cfg.Retry = config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 2}
	cfg.Models = config.ModelsConfig{RefreshSeconds: 3600}
	cfg.Performance = config.PerformanceConfig{MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeoutSeconds: 90, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1}
	cfg.WebUI = config.WebUIConfig{Username: "admin", SessionTTLMinutes: 60}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root, cancel := context.WithCancel(context.Background())
	cancel()
	manager, err := gateway.RuntimeManager(root, "", cfg, logger, telemetry.Monitor(), telemetry.LogHub(100), config.SecretRedactor(), new(slog.LevelVar))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	return New(manager, nil, nil, logger)
}

// withSession seeds one live session, which is what login would store.
func withSession(t *testing.T, server *Server) {
	t.Helper()
	cfg := server.manager.Config()
	server.mu.Lock()
	server.sessions[tokenDigest(testSessionToken)] = adminSession{
		Username: cfg.WebUI.Username, AuthVersion: config.Fingerprint(cfg.WebUI.PasswordHash),
		CSRF: testCSRFToken, Expires: time.Now().Add(time.Hour),
	}
	server.mu.Unlock()
}

func TestHandleCheckProxyAcceptsConfiguredIDAndDirectValue(t *testing.T) {
	server := proxyTestServer(t, config.Config{})
	proxyID := config.Fingerprint("socks5://127.0.0.1:2#pool")
	for _, body := range []string{
		`{"id":"` + proxyID + `"}`,
		`{"id":"` + config.Fingerprint("direct") + `"}`,
		`{"value":"direct"}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(body))
		server.handleCheckProxy(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("check %s returned %d: %s", body, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("check %s Cache-Control = %q, want no-store", body, recorder.Header().Get("Cache-Control"))
		}
		var result gateway.ProxyCheckResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.Reason == "" || result.State == "" {
			t.Errorf("check %s returned no verdict: %+v", body, result)
		}
	}
}

func TestHandleCheckProxyChecksProxyFileEntryByID(t *testing.T) {
	dir := t.TempDir()
	entry := "http://127.0.0.1:1#pool"
	if err := os.WriteFile(filepath.Join(dir, "proxies.txt"), []byte(entry+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := `{
		"listen": "127.0.0.1:18080", "server_keys": ["server-secret"], "zen_keys": ["zen-secret"],
		"proxyfile": "proxies.txt", "prefer": "zen",
		"upstream": {"zen": "http://127.0.0.1:1"},
		"retry": {"max_attempts": 1, "timeout_seconds": 2},
		"models": {"refresh_seconds": 3600},
		"performance": {"max_idle_conns": 1, "max_idle_conns_per_host": 1, "idle_conn_timeout_seconds": 90, "connect_timeout_seconds": 2, "failure_cooldown_seconds": 1},
		"webui": {"listen": "127.0.0.1:18081", "username": "admin", "session_ttl_minutes": 60}
	}`
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	server := proxyTestServer(t, cfg)
	recorder := httptest.NewRecorder()
	body := `{"id":"` + config.Fingerprint(entry) + `"}`
	server.handleCheckProxy(recorder, httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxyfile check returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleCheckProxyRejectsInvalidInputWithoutChecking(t *testing.T) {
	server := proxyTestServer(t, config.Config{})
	for _, body := range []string{
		`{}`,
		`{"id":"unknown"}`,
		`{"id":"a","value":"direct"}`,
		`{"value":"ftp://proxy.invalid"}`,
		`{"value":"` + strings.Repeat("x", 16<<10+1) + `"}`,
		`{"value":"direct","extra":1}`,
		`{"value":"direct"}{"value":"direct"}`,
	} {
		recorder := httptest.NewRecorder()
		server.handleCheckProxy(recorder, httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("invalid body returned %d, want 400: %.160s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandleCheckProxyRedactsCredentials(t *testing.T) {
	server := proxyTestServer(t, config.Config{})
	const password = "s3cret-password"
	for _, body := range []string{
		`{"value":"http://user:` + password + `@127.0.0.1:1"}`,
		`{"value":"ftp://user:` + password + `@127.0.0.1:1"}`,
	} {
		recorder := httptest.NewRecorder()
		server.handleCheckProxy(recorder, httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(body)))
		if strings.Contains(recorder.Body.String(), password) {
			t.Errorf("response for %s leaks the credential: %s", body, recorder.Body.String())
		}
	}
}

func TestHandleCheckProxyRejectsExcessConcurrency(t *testing.T) {
	server := proxyTestServer(t, config.Config{})
	for range cap(server.proxyChecks) {
		server.proxyChecks <- struct{}{}
	}
	defer func() {
		for range cap(server.proxyChecks) {
			<-server.proxyChecks
		}
	}()
	recorder := httptest.NewRecorder()
	server.handleCheckProxy(recorder, httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(`{"value":"direct"}`)))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("busy check returned %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("busy check sent no Retry-After")
	}
}

func TestHandlerMountsProxyCheckBehindAuthAndCSRF(t *testing.T) {
	server := proxyTestServer(t, config.Config{})
	withSession(t, server)
	const body = `{"value":"direct"}`
	cases := []struct {
		name   string
		method string
		cookie bool
		csrf   string
		want   int
	}{
		{name: "no session", method: http.MethodPost, csrf: testCSRFToken, want: http.StatusUnauthorized},
		{name: "no csrf token", method: http.MethodPost, cookie: true, want: http.StatusForbidden},
		{name: "wrong csrf token", method: http.MethodPost, cookie: true, csrf: "wrong", want: http.StatusForbidden},
		{name: "wrong method", method: http.MethodGet, cookie: true, csrf: testCSRFToken, want: http.StatusNotFound},
		{name: "authorized", method: http.MethodPost, cookie: true, csrf: testCSRFToken, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/api/proxies/check", strings.NewReader(body))
			if tc.cookie {
				request.AddCookie(&http.Cookie{Name: adminCookieName, Value: testSessionToken})
			}
			if tc.csrf != "" {
				request.Header.Set("X-CSRF-Token", tc.csrf)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}
