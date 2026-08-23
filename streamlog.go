package main

// ponytail: P0 observability for mid-stream truncation diagnosis.
// Logs one INFO line per upstream stream end: outcome (upstream_clean /
// upstream_broken / client_write_error), bytes delivered, max inter-chunk gap
// and total duration. The max-gap figure exists to confirm or refute the
// "Cloudflare idle timeout during long reasoning pauses" hypothesis (~100s gaps).
// Upgrade path: once root cause shares are known, add peek-retry or keepalive.

import (
	"context"
	"io"
	"log/slog"
	"time"
)

type streamEndObserver struct {
	reader   io.Reader
	started  time.Time
	lastRead time.Time
	maxGap   time.Duration
	bytes    int64
	readErr  error // first non-EOF error returned by the upstream body read side
}

func newStreamEndObserver(reader io.Reader) *streamEndObserver {
	now := time.Now()
	return &streamEndObserver{reader: reader, started: now, lastRead: now}
}

func (s *streamEndObserver) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if n > 0 {
		now := time.Now()
		if gap := now.Sub(s.lastRead); gap > s.maxGap {
			s.maxGap = gap
		}
		s.lastRead = now
		s.bytes += int64(n)
	}
	if err != nil && err != io.EOF && s.readErr == nil {
		s.readErr = err
	}
	return n, err
}

func (g *Gateway) logStreamEnd(ids requestIDs, model string, route modelRoute, external Protocol, obs *streamEndObserver, copyErr error) {
	if g == nil || g.logger == nil || obs == nil {
		return
	}
	outcome := "upstream_clean"
	switch {
	case obs.readErr != nil:
		outcome = "upstream_broken"
	case copyErr != nil:
		outcome = "client_write_error"
	}
	attrs := []any{
		"request_id", ids.Request,
		"model", model,
		"tier", string(route.Tier),
		"native_protocol", string(route.Protocol),
		"external_protocol", string(external),
		"outcome", outcome,
		"bytes", obs.bytes,
		"max_gap_ms", obs.maxGap.Milliseconds(),
		"duration_ms", time.Since(obs.started).Milliseconds(),
	}
	if obs.readErr != nil {
		attrs = append(attrs, "read_error", obs.readErr.Error())
	}
	g.logger.Log(context.Background(), slog.LevelInfo, "upstream stream ended", attrs...)
}
