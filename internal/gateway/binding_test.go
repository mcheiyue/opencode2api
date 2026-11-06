package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestNodePoolPerKeyBindingAndRecovery(t *testing.T) {
	transports := &proxyTransports{items: []*proxyTransport{
		{index: 0, name: "http://127.0.0.1:18090"},
		{index: 1, name: "http://127.0.0.1:18091"},
		{index: 2, name: "http://127.0.0.1:18092"},
	}}
	for _, transport := range transports.items {
		transport.healthy.Store(true)
	}
	bindings := map[string]string{
		config.Fingerprint("key-alpha"): transports.items[1].name,
		"key-gamma":                     transports.items[0].name,
	}
	pool, err := nodePool([]string{"key-alpha", "key-beta", "key-gamma"}, transports, time.Second, bindings)
	if err != nil {
		t.Fatal(err)
	}
	alpha, beta, gamma := pool.nodes[0], pool.nodes[1], pool.nodes[2]
	if !alpha.bound || alpha.proxyIndex.Load() != 1 || beta.bound || beta.proxyIndex.Load() != 1 || !gamma.bound || gamma.proxyIndex.Load() != 0 {
		t.Fatal("explicit and automatic bindings were not assigned independently")
	}
	transports.items[1].healthy.Store(false)
	if moved := pool.RebindProxy(1); moved != 1 || alpha.proxyIndex.Load() != 1 || beta.proxyIndex.Load() == 1 {
		t.Fatalf("proxy failure moved explicit binding or left unbound key behind: moved=%d", moved)
	}
	alpha.failures.Store(3)
	alpha.cooldownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	transports.items[1].healthy.Store(true)
	if moved := pool.RestoreProxy(1); moved != 1 || beta.proxyIndex.Load() != 1 || alpha.failures.Load() != 0 || alpha.cooldownUntil.Load() != 0 {
		t.Fatalf("proxy recovery failed to restore automatic key and clear bound-key penalty: moved=%d", moved)
	}
	reordered, err := nodePool([]string{"key-gamma", "key-alpha", "key-beta"}, transports, time.Second, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if node := reordered.NodeByID(config.Fingerprint("key-alpha")); node == nil || node.proxyIndex.Load() != 1 {
		t.Fatal("key reordering shifted explicit binding")
	}
	anon := anonymousPool(true, transports, time.Second, anonymousBackoff{})
	cursor := anon.CursorFor("")
	seen := map[string]bool{}
	for range transports.items {
		if node := cursor.Next(); node != nil {
			seen[node.proxy.name] = true
		}
	}
	if len(seen) != len(transports.items) {
		t.Fatalf("key bindings restricted anonymous proxy set: %v", seen)
	}
}

func TestKeyStatusesReportsBindingWithoutSecret(t *testing.T) {
	transports := &proxyTransports{items: []*proxyTransport{{index: 0, name: "direct"}}}
	transports.items[0].healthy.Store(true)
	const secret = "super-secret-zen-key-xyz99"
	pool, err := nodePool([]string{secret}, transports, time.Second, map[string]string{config.Fingerprint(secret): "direct"})
	if err != nil {
		t.Fatal(err)
	}
	statuses := keyStatuses("zen", pool)
	if len(statuses) != 1 || !statuses[0].Bound || statuses[0].ID != "xyz99" {
		t.Fatalf("unexpected public key status: %+v", statuses)
	}
}

func TestGatewayBoundKeyFailsClosed(t *testing.T) {
	var boundCalls, otherCalls atomic.Int32
	var fail atomic.Bool
	bound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boundCalls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"bound proxy"}}]}`)
	}))
	defer bound.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"other proxy"}}]}`)
	}))
	defer other.Close()
	gw, err := New(config.Config{
		ServerKeys: []string{"local-test"}, ZenKeys: []string{"bound-key"},
		Proxies:       []string{bound.URL, other.URL},
		ProxyBindings: map[string]string{config.Fingerprint("bound-key"): bound.URL},
		Upstream:      config.UpstreamConfig{Zen: bound.URL},
		Retry:         config.RetryConfig{MaxAttempts: 2, TimeoutSeconds: 3},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := models.Route{ID: "paid-model", Tier: config.TierZen, Protocol: wire.Chat}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body := map[config.Tier][]byte{config.TierZen: []byte(`{"model":"paid-model","messages":[{"role":"user","content":"hello"}]}`)}
	gw.transports.items[0].healthy.Store(false)
	gw.rebindUnavailableProxy(gw.transports.items[0], true)
	fail.Store(true)
	resp, _, err := gw.doUpstream(ctx, route, body, identity.RequestIDs{Session: "binding-test"})
	if err != nil || resp == nil {
		t.Fatalf("missing bound-proxy failure response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || boundCalls.Load() != 2 || otherCalls.Load() != 0 {
		t.Fatalf("unavailable explicit proxy did not fail closed: status=%d bound=%d other=%d", resp.StatusCode, boundCalls.Load(), otherCalls.Load())
	}
	fail.Store(false)
	gw.transports.items[0].healthy.Store(true)
	gw.restoreProxy(gw.transports.items[0])
	resp, _, err = gw.doUpstream(ctx, route, body, identity.RequestIDs{Session: "binding-test"})
	if err != nil || resp == nil {
		t.Fatalf("recovered binding could not serve request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || boundCalls.Load() != 3 || otherCalls.Load() != 0 {
		t.Fatalf("recovered key used wrong proxy: status=%d bound=%d other=%d", resp.StatusCode, boundCalls.Load(), otherCalls.Load())
	}
}
