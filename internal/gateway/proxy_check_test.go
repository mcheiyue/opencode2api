package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
)

// checkRuntime builds a controller whose current runtime carries cfg and gw, the
// two fields CheckProxy reads.
func checkRuntime(cfg config.Config, gw *Gateway) *RuntimeController {
	controller := &RuntimeController{}
	controller.current.Store(&gatewayRuntime{config: cfg, gateway: gw})
	return controller
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// connectProxy is an HTTP proxy that answers CONNECT, then serves the tunneled
// request with respond and reports it to seen. The check client always tunnels,
// even to a plain-HTTP target, so this is the shape a proxy check exercises.
func connectProxy(t *testing.T, respond func(*http.Request) *http.Response, seen func(*http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy received %s %s, want CONNECT", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack proxy connection: %v", err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		tunneled, err := http.ReadRequest(buffered.Reader)
		if err != nil {
			t.Errorf("read tunneled request: %v", err)
			return
		}
		seen(tunneled)
		if err := respond(tunneled).Write(conn); err != nil {
			t.Errorf("write tunneled response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}

func assertNoServiceCredentials(t *testing.T, r *http.Request) {
	t.Helper()
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key", "Proxy-Authorization"} {
		if value := r.Header.Get(name); value != "" {
			t.Errorf("check sent %s: %q", name, value)
		}
	}
}

func TestCheckProxyDirectSendsNoCredentialsAndChangesNoState(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assertNoServiceCredentials(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("upstream saw %s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	defer upstream.Close()

	cfg := config.Config{
		ServerKeys: []string{"server-secret"}, ZenKeys: []string{"zen-secret"}, GoKeys: []string{"go-secret"},
		Proxies:  []string{"http://127.0.0.1:1"},
		Upstream: config.UpstreamConfig{Zen: upstream.URL},
		Retry:    config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5},
	}
	gw, err := New(cfg, discardLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	production := gw.transports.items[0]
	production.healthy.Store(false)

	result := checkRuntime(cfg, gw).CheckProxy(context.Background(), "direct")
	if !result.OK || result.State != "available" || result.Reason != "ok" || result.Status != http.StatusOK {
		t.Fatalf("unexpected direct check result: %+v", result)
	}
	if requests.Load() != 1 {
		t.Fatalf("upstream requests = %d, want 1", requests.Load())
	}
	if production.healthy.Load() || production.checking.Load() {
		t.Fatal("check mutated production proxy health or in-flight state")
	}
}

func TestCheckProxyUsesConfiguredProxyAndRejectsRedirects(t *testing.T) {
	cfg := config.Config{Upstream: config.UpstreamConfig{Zen: "http://upstream.invalid"}}
	cases := []struct {
		name    string
		respond func(*http.Request) *http.Response
		want    ProxyCheckResult
	}{
		{name: "ok", respond: func(*http.Request) *http.Response { return textResponse(http.StatusOK, "{}") },
			want: ProxyCheckResult{OK: true, State: "available", Reason: "ok", Status: http.StatusOK}},
		{name: "redirect", respond: func(*http.Request) *http.Response {
			response := textResponse(http.StatusFound, "")
			response.Header.Set("Location", "http://tracker.invalid/followed")
			return response
		}, want: ProxyCheckResult{State: "rejected", Reason: "upstream_responded", Status: http.StatusFound}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var followed atomic.Int32
			tracker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
			defer tracker.Close()
			var seen atomic.Int32
			proxy := connectProxy(t, tc.respond, func(r *http.Request) {
				seen.Add(1)
				assertNoServiceCredentials(t, r)
				if r.URL.Path != "/v1/models" || r.Host != "upstream.invalid" {
					t.Errorf("tunneled request = %s host %q, want /v1/models on upstream.invalid", r.URL.Path, r.Host)
				}
			})
			for _, value := range []string{proxy.URL, proxy.URL + "#pool"} {
				seen.Store(0)
				result := checkRuntime(cfg, nil).CheckProxy(context.Background(), value)
				if result.OK != tc.want.OK || result.State != tc.want.State || result.Reason != tc.want.Reason || result.Status != tc.want.Status {
					t.Fatalf("%s check result = %+v, want %+v", value, result, tc.want)
				}
				if seen.Load() != 1 {
					t.Fatalf("%s reached the configured proxy %d times, want 1", value, seen.Load())
				}
			}
			if followed.Load() != 0 {
				t.Fatalf("check followed a redirect %d times", followed.Load())
			}
		})
	}
}

func TestCheckProxyIsContextBounded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	cfg := config.Config{Upstream: config.UpstreamConfig{Zen: upstream.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	result := checkRuntime(cfg, nil).CheckProxy(ctx, "direct")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled check took %s, want an immediate return", elapsed)
	}
	if result.OK || result.State != "inconclusive" || result.Reason != "canceled" {
		t.Fatalf("canceled check result = %+v", result)
	}
}

func TestCheckProxyRejectsInvalidAndUnavailableRuntime(t *testing.T) {
	cfg := config.Config{Upstream: config.UpstreamConfig{Zen: "http://upstream.invalid"}}
	cases := []struct {
		name    string
		runtime *RuntimeController
		value   string
		reason  string
	}{
		{name: "invalid proxy", runtime: checkRuntime(cfg, nil), value: "ftp://proxy.invalid", reason: "invalid_proxy"},
		{name: "unavailable runtime", runtime: &RuntimeController{}, value: "direct", reason: "runtime_unavailable"},
		{name: "missing upstream", runtime: checkRuntime(config.Config{}, nil), value: "direct", reason: "runtime_unavailable"},
	}
	for _, tc := range cases {
		result := tc.runtime.CheckProxy(context.Background(), tc.value)
		if result.State != "inconclusive" || result.Reason != tc.reason {
			t.Errorf("%s result = %+v, want inconclusive/%s", tc.name, result, tc.reason)
		}
	}
}

func TestProxyCheckStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   ProxyCheckResult
	}{
		{http.StatusOK, ProxyCheckResult{OK: true, State: "available", Reason: "ok", Status: http.StatusOK}},
		{http.StatusUnauthorized, ProxyCheckResult{OK: true, State: "available", Reason: "authentication_required", Status: http.StatusUnauthorized}},
		{http.StatusTooManyRequests, ProxyCheckResult{State: "limited", Reason: "rate_limited", Status: http.StatusTooManyRequests}},
		{http.StatusForbidden, ProxyCheckResult{State: "rejected", Reason: "upstream_responded", Status: http.StatusForbidden}},
	}
	for _, tc := range cases {
		if got := proxyCheckStatus(tc.status); got != tc.want {
			t.Errorf("proxyCheckStatus(%d) = %+v, want %+v", tc.status, got, tc.want)
		}
	}
}
