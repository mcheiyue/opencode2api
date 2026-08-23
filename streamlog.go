package main

// ponytail: P0+P1 observability for mid-stream truncation diagnosis.
// Logs one INFO line per upstream stream end: outcome (upstream_clean /
// upstream_broken / client_write_error), bytes delivered, max inter-chunk gap,
// total duration, and completion status (completed vs premature_close).
// The completion field distinguishes "upstream sentfinish_reason"
// from "clean TCP close without terminal marker" — the latter is upstream
// silently abandoning generation mid-stream.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"
)

type streamEndObserver struct {
	reader       io.Reader
	started      time.Time
	lastRead     time.Time
	maxGap       time.Duration
	bytes        int64
	readErr      error // first non-EOF error returned by the upstream body read side
	hasContent   bool   // saw at least one non-empty content/reasoning/tool delta
	sawTerminal  bool   // finish_reason
	protocol     Protocol
	sseBuffer    []byte
	parser       *bridgeStreamParser
}

func newStreamEndObserver(reader io.Reader, protocol Protocol) *streamEndObserver {
	now := time.Now()
	return &streamEndObserver{
		reader:    reader,
		started:    now,
		lastRead:   now,
		protocol:   protocol,
		parser: &bridgeStreamParser{
			protocol:          protocol,
			tools:             map[string]bool{},
			toolIDs:           map[string]string{},
			toolNames:         map[string]string{},
			responseArgs:      map[string]bool{},
			responseReasoning: map[string]bool{},
		},
	}
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
		s.scan(p[:n])
	}
	if err != nil && err != io.EOF && s.readErr == nil {
		s.readErr = err
	}
	return n, err
}

// scan feeds raw bytes through a minimal SSE frame parser, tracking whether
// any meaningful content (text, reasoning, tool call) or terminal marker
// ([DONE], finish_reason) has been observed.
func (s *streamEndObserver) scan(data []byte) {
	s.sseBuffer = append(s.sseBuffer, data...)
	for {
		index, width := nextSSEBoundary(s.sseBuffer)
		if index < 0 {
			break
		}
		frame := append([]byte(nil), s.sseBuffer[:index+width]...)
		s.sseBuffer = s.sseBuffer[index+width:]
		s.consumeFrame(frame)
	}
}

func (s *streamEndObserver) consumeFrame(frame []byte) {
	_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		events, err := s.parser.Parse(eventName, data)
		if err != nil {
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "text", "reasoning", "tool_delta":
				if event.Text != "" {
					s.hasContent = true
				}
			case "done":
				s.sawTerminal = true
			case "finish":
				s.sawTerminal = true
			}
		}
		return nil
	})
}

// finish processes any remaining buffered bytes (called after stream ends).
func (s *streamEndObserver) finish() {
	if len(s.sseBuffer) > 0 {
		s.consumeFrame(s.sseBuffer)
		s.sseBuffer = nil
	}
}

func (g *Gateway) logStreamEnd(ids requestIDs, model string, route modelRoute, external Protocol, obs *streamEndObserver, copyErr error) {
	if g == nil || g.logger == nil || obs == nil {
		return
	}
	obs.finish()

	outcome := "upstream_clean"
	switch {
	case obs.readErr != nil:
		outcome = "upstream_broken"
	case copyErr != nil:
		outcome = "client_write_error"
	}

	// completion: did the upstream send a proper terminal marker?
	completion := "completed"
	if !obs.sawTerminal {
		if obs.hasContent {
			completion = "premature_close"
		} else {
			completion = "empty_response"
		}
	}

	attrs := []any{
		"request_id", ids.Request,
		"model", model,
		"tier", string(route.Tier),
		"native_protocol", string(route.Protocol),
		"external_protocol", string(external),
		"outcome", outcome,
		"completion", completion,
		"bytes", obs.bytes,
		"max_gap_ms", obs.maxGap.Milliseconds(),
		"duration_ms", time.Since(obs.started).Milliseconds(),
	}
	if obs.readErr != nil {
		attrs = append(attrs, "read_error", obs.readErr.Error())
	}
	g.logger.Log(context.Background(), slog.LevelInfo, "upstream stream ended", attrs...)
}
