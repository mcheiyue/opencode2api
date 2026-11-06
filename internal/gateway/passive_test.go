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
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestActiveProbesRequireOptIn(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "passive"
		if active {
			name = "opt-in"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := &RuntimeController{root: context.Background(), logger: logger}
			runtime, err := manager.build(config.Config{
				ServerKeys: []string{"local-test"}, Anonymous: true,
				Upstream: config.UpstreamConfig{Anonymous: server.URL, Zen: server.URL},
				Models:   config.ModelsConfig{ActiveProbes: active},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.current.Store(runtime)
			runtime.gateway.catalog.ReplaceWithCapabilities([]string{"qa-free"}, nil,
				map[config.Tier]map[string]wire.Protocol{config.TierZen: {"qa-free": wire.Chat}}, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			manager.checkFreeModels(ctx)
			want := int32(0)
			if active {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("synthetic inference calls = %d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestPassiveObservationRejectsOnlyAuthoritativeUnavailable(t *testing.T) {
	registry, err := models.AvailabilityStore("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, status := range []int{400, 404, 429, 403, 500, 502, 503} {
		if err := registry.RecordPassiveObservation("qa-free", false, status, `{"error":"generic failure"}`, "anonymous", now); err != nil {
			t.Fatal(err)
		}
		if registry.Get("qa-free").Disabled {
			t.Fatalf("generic HTTP %d disabled model", status)
		}
	}
	if err := registry.RecordPassiveObservation("qa-free", false, 404, `{"error":{"code":"model_not_found","message":"The model qa-free does not exist"}}`, "anonymous", now); err != nil {
		t.Fatal(err)
	}
	if item := registry.Get("qa-free"); !item.Disabled || item.Reason != "model_unavailable" {
		t.Fatalf("authoritative removal was ignored: %+v", item)
	}
	if err := registry.RecordPassiveObservation("qa-free", true, 200, "", "anonymous", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if registry.Get("qa-free").Disabled {
		t.Fatal("real success did not re-enable model")
	}
}

type catalogRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip catalogRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func localCatalogRefresh(t *testing.T, handler http.Handler) (*Gateway, context.Context) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	gw, err := New(config.Config{
		ServerKeys: []string{"local-test"}, Anonymous: true,
		Upstream: config.UpstreamConfig{Anonymous: server.URL, Zen: server.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = catalogRoundTripper(func(request *http.Request) (*http.Response, error) {
		status, body := http.StatusNotFound, ""
		if request.URL.String() == models.CapabilitiesURL {
			status = http.StatusOK
			body = `{"opencode":{"api":"https://opencode.ai/zen","npm":"@ai-sdk/openai-compatible","models":{"qa-free":{"id":"qa-free"}}}}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return gw, ctx
}

func TestCatalogRefreshPreservesLastKnownGood(t *testing.T) {
	var fail atomic.Bool
	gw, ctx := localCatalogRefresh(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qa-free"}]}`)
	}))
	gw.RefreshCatalog(ctx)
	if got := gw.catalog.List(); len(got) != 1 || got[0] != "qa-free" {
		t.Fatalf("initial discovery = %v", got)
	}
	fail.Store(true)
	gw.RefreshCatalog(ctx)
	if got := gw.catalog.List(); len(got) != 1 || got[0] != "qa-free" {
		t.Fatalf("failed refresh lost working model: %v", got)
	}
	if _, err := gw.catalog.Route("qa-free", false, false, true); err != nil {
		t.Fatalf("failed refresh lost route capabilities: %v", err)
	}
}

func TestCatalogRefreshSkipsOverlappingExecutions(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}, 5), make(chan struct{})
	gw, ctx := localCatalogRefresh(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"qa-free"}]}`)
	}))
	defer close(release)
	done := make(chan struct{}, 5)
	run := func() { gw.RefreshCatalog(ctx); done <- struct{}{} }
	go run()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first refresh never reached upstream")
	}
	for range 4 {
		go run()
	}
	for range 4 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("overlapping refreshes waited instead of being skipped")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("overlapping refreshes made %d upstream calls", calls.Load())
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("initial refresh did not finish")
	}
}
