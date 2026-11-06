package warp

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func testPool(exits []*exit, create func(context.Context) (*exit, error)) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		ctx:      ctx,
		cancel:   cancel,
		logger:   slogLogger{},
		size:     len(exits),
		preload:  len(exits),
		active:   exits,
		newExit:  create,
		reserved: make(map[string]bool),
		cooling:  make(map[string]time.Time),
		changed:  make(chan struct{}),
	}
	for _, item := range exits {
		p.reserved[item.egressKey()] = true
	}
	return p
}

func testExit(id, address string) *exit {
	return &exit{id: id, stack: StackIPv6, egress: netip.MustParseAddr(address)}
}

func TestPoolReportWithRetryAfterCoolsOnlyRejectedEgressScope(t *testing.T) {
	bad := testExit("bad", "2001:db8:1::10")
	other := testExit("other", "2001:db8:2::10")
	p := testPool([]*exit{bad, other}, func(context.Context) (*exit, error) {
		return nil, errors.New("replacement disabled for test")
	})
	defer p.Close()

	retryAfter := 2 * time.Hour
	started := time.Now()
	p.ReportWithRetryAfter(&Lease{pool: p, exit: bad}, http.StatusTooManyRequests, nil, retryAfter)

	p.mu.Lock()
	until := p.cooling[bad.egressKey()]
	_, otherCooling := p.cooling[other.egressKey()]
	p.mu.Unlock()
	if !until.After(time.Now().Add(retryAfter - time.Second)) {
		t.Fatalf("rejected egress cooldown ends at %s, want at least %s after report near %s", until, retryAfter, started)
	}
	if otherCooling {
		t.Fatalf("distinct egress scope %s was cooled", other.egressKey())
	}
	if !p.HasUntriedActive(map[string]bool{bad.egressKey(): true}) {
		t.Fatal("distinct active egress should remain usable after one scope is cooled")
	}
	lease, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	if lease.EgressKey() != other.egressKey() {
		t.Fatalf("Acquire() returned egress %s, want distinct active scope %s", lease.EgressKey(), other.egressKey())
	}
}

func TestPoolHasUntriedActiveTurnsFalseAfterEveryRemainingScopeIsTried(t *testing.T) {
	first := testExit("first", "2001:db8:1::10")
	second := testExit("second", "2001:db8:2::10")
	last := testExit("last", "2001:db8:3::10")
	p := testPool([]*exit{first, second, last}, func(context.Context) (*exit, error) {
		return nil, errors.New("replacement disabled for test")
	})
	defer p.Close()

	p.ReportWithRetryAfter(&Lease{pool: p, exit: first}, http.StatusTooManyRequests, nil, 0)
	tried := map[string]bool{second.egressKey(): true}
	if !p.HasUntriedActive(tried) {
		t.Fatal("remaining distinct egress should be available before the last scope is tried")
	}

	p.ReportWithRetryAfter(&Lease{pool: p, exit: second}, http.StatusTooManyRequests, nil, 0)
	tried[last.egressKey()] = true
	if p.HasUntriedActive(tried) {
		t.Fatal("no untried active egress should remain after every remaining scope is tried")
	}
}

func TestPoolStartsReplacementForEveryCoolingExit(t *testing.T) {
	first := testExit("first", "2001:db8:1::10")
	second := testExit("second", "2001:db8:2::10")
	started := make(chan string, 2)
	var replacements atomic.Uint32
	p := testPool([]*exit{first, second}, func(ctx context.Context) (*exit, error) {
		index := replacements.Add(1)
		address := "2001:db8:3::10"
		if index == 2 {
			address = "2001:db8:4::10"
		}
		replacement := testExit("replacement", address)
		started <- replacement.id
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return replacement, nil
		}
	})
	defer p.Close()

	p.Report(&Lease{pool: p, exit: first}, http.StatusTooManyRequests, nil)
	p.Report(&Lease{pool: p, exit: second}, http.StatusTooManyRequests, nil)

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for background replacement")
		}
	}
}
