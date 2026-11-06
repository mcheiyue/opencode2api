package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const quotaExhaustedBody = `{"error":{"code":null,"message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go","param":null,"type":"upstream_error"}}`

// quotaGate remembers that the free tier asked us to come back after a long
// Retry-After, so later requests answer immediately instead of spending the
// attempt budget rebuilding WARP exits that cannot succeed.
type quotaGate struct {
	mu    sync.Mutex
	until time.Time
}

func (q *quotaGate) blocked() (time.Duration, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.remainingLocked()
}

func (q *quotaGate) open(d time.Duration) {
	if d <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if until := time.Now().Add(d); until.After(q.until) {
		q.until = until
	}
}

func (q *quotaGate) exhaustedUntil() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	wait, ok := q.remainingLocked()
	if !ok {
		return ""
	}
	return time.Now().Add(wait).UTC().Format(time.RFC3339)
}

func (q *quotaGate) remainingLocked() (time.Duration, bool) {
	if q.until.IsZero() {
		return 0, false
	}
	wait := time.Until(q.until)
	if wait <= 0 {
		return 0, false
	}
	return wait, true
}

func (g *Gateway) capRetryAfter(resp *http.Response) {
	limit := time.Duration(g.cfg.Retry.MaxRetryAfterSeconds) * time.Second
	if limit <= 0 || resp == nil {
		return
	}
	wait := parseRetryAfter(resp.Header.Get("Retry-After"))
	if wait <= limit {
		return
	}
	seconds := int((limit + time.Second - 1) / time.Second)
	resp.Header.Set("Retry-After", strconv.Itoa(seconds))
}

func (g *Gateway) quotaGateWait() (time.Duration, bool) {
	if g.quota == nil {
		return 0, false
	}
	return g.quota.blocked()
}

func (g *Gateway) observeQuotaRejection(resp *http.Response) {
	if g.quota == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	threshold := time.Duration(g.cfg.Retry.QuotaGateSeconds) * time.Second
	if threshold <= 0 {
		return
	}
	wait := parseRetryAfter(resp.Header.Get("Retry-After"))
	if wait < threshold {
		return
	}
	g.quota.open(wait)
	g.logger.Warn("free tier quota exhausted; failing requests fast until it recovers", "component", "upstream", "event", "quota_gate_open", "retry_after", wait.String())
}

func (g *Gateway) quotaExhaustedUntil() string {
	if g.quota == nil {
		return ""
	}
	return g.quota.exhaustedUntil()
}

// quotaExhaustedResponse builds the 429 the gate answers with. It carries the
// remaining wait so clients and CPA cool down for the real recovery time.
func quotaExhaustedResponse(wait time.Duration) *http.Response {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	header := make(http.Header, 2)
	header.Set("Content-Type", "application/json")
	header.Set("Retry-After", strconv.Itoa(seconds))
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(quotaExhaustedBody)),
	}
}
