package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

func TestQuotaGateBlocksUntilDeadline(t *testing.T) {
	gate := &quotaGate{}
	if _, blocked := gate.blocked(); blocked {
		t.Fatal("fresh gate must not block")
	}

	gate.open(2 * time.Hour)
	wait, blocked := gate.blocked()
	if !blocked {
		t.Fatal("opened gate must block")
	}
	if wait <= 0 || wait > 2*time.Hour {
		t.Fatalf("remaining %s outside (0, 2h]", wait)
	}

	gate.mu.Lock()
	gate.until = time.Now().Add(-time.Second)
	gate.mu.Unlock()
	if _, blocked := gate.blocked(); blocked {
		t.Fatal("expired gate must not block")
	}
	if gate.exhaustedUntil() != "" {
		t.Fatal("expired gate must report no deadline")
	}

	gate.open(0)
	gate.open(-time.Second)
	if _, blocked := gate.blocked(); blocked {
		t.Fatal("non-positive open must be ignored")
	}
}

func TestQuotaGateKeepsLongestDeadline(t *testing.T) {
	gate := &quotaGate{}
	gate.open(2 * time.Hour)
	gate.open(time.Minute)

	wait, blocked := gate.blocked()
	if !blocked {
		t.Fatal("gate must block")
	}
	if wait < time.Hour {
		t.Fatalf("shorter open shrank the deadline: %s", wait)
	}
}

func TestQuotaExhaustedResponse(t *testing.T) {
	resp := quotaExhaustedResponse(90 * time.Minute)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5400" {
		t.Fatalf("retry-after = %q", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()

	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Error.Message == "" {
		t.Fatalf("body lost the upstream message: %s", raw)
	}
}

func TestQuotaGateWaitGuardsNil(t *testing.T) {
	g := &Gateway{}
	if _, blocked := g.quotaGateWait(); blocked {
		t.Fatal("nil gate must not block")
	}
	if g.quotaExhaustedUntil() != "" {
		t.Fatal("nil gate must report no deadline")
	}
	g.observeQuotaRejection(nil)
}

func TestWarpLaneShortCircuitsWhenQuotaGateOpen(t *testing.T) {
	gate := &quotaGate{}
	gate.open(time.Hour)
	g := &Gateway{quota: gate}

	resp, err, attempts := g.doWarpAnonymousUpstream(context.Background(), models.Route{}, nil, identity.RequestIDs{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0", attempts)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("short-circuit response lost Retry-After")
	}
	resp.Body.Close()
}

func TestObserveQuotaRejectionUsesThreshold(t *testing.T) {
	newGateway := func(threshold int) *Gateway {
		return &Gateway{
			quota:  &quotaGate{},
			cfg:    config.Config{Retry: config.RetryConfig{QuotaGateSeconds: threshold}},
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	response := func(retryAfter string) *http.Response {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{retryAfter}}}
	}

	above := newGateway(300)
	above.observeQuotaRejection(response("3600"))
	if _, blocked := above.quota.blocked(); !blocked {
		t.Fatal("long Retry-After must arm the gate")
	}

	below := newGateway(300)
	below.observeQuotaRejection(response("60"))
	if _, blocked := below.quota.blocked(); blocked {
		t.Fatal("short Retry-After must not arm the gate")
	}

	disabled := newGateway(0)
	disabled.observeQuotaRejection(response("3600"))
	if _, blocked := disabled.quota.blocked(); blocked {
		t.Fatal("threshold 0 must disable the gate")
	}
}
