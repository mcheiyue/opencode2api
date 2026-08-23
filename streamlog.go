package main

// P0 observability for streamed upstream responses. This file deliberately
// does not retry or continue a response after any bytes reach the client.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"
)

type streamEndObserver struct {
	reader         io.Reader
	started        time.Time
	lastRead       time.Time
	maxGap         time.Duration
	bytes          int64
	chunks         int64
	readErr        error
	hasContent     bool
	sawTerminal    bool
	terminalMarker string
	reasoningBytes int64
	visibleBytes   int64
	toolBytes      int64
	sseFrames      int64
	parseErrors    int64
	lastEventName  string
	lastEventKind  string
	sseBuffer      []byte
	parser         *bridgeStreamParser
}

func newStreamEndObserver(reader io.Reader, protocol Protocol) *streamEndObserver {
	now := time.Now()
	return &streamEndObserver{
		reader:   reader,
		started:  now,
		lastRead: now,
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
		s.chunks++
		s.scan(p[:n])
	}
	if err != nil && err != io.EOF && s.readErr == nil {
		s.readErr = err
	}
	return n, err
}

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
	s.sseFrames++
	_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		s.lastEventName = eventName
		events, err := s.parser.Parse(eventName, data)
		if err != nil {
			s.parseErrors++
			s.lastEventKind = "parse_error"
			return nil
		}
		for _, event := range events {
			s.lastEventKind = event.Kind
			switch event.Kind {
			case "reasoning":
				if event.Text != "" {
					s.hasContent = true
					s.reasoningBytes += int64(len(event.Text))
				}
			case "text":
				if event.Text != "" {
					s.hasContent = true
					s.visibleBytes += int64(len(event.Text))
				}
			case "tool_start":
				s.hasContent = true
			case "tool_delta":
				s.hasContent = true
				s.toolBytes += int64(len(event.Text))
			case "done":
				s.sawTerminal = true
				s.terminalMarker = "done"
			case "finish":
				s.sawTerminal = true
				s.terminalMarker = "finish"
				if event.Stop != "" {
					s.terminalMarker += ":" + event.Stop
				}
			}
		}
		return nil
	})
}

func (s *streamEndObserver) finish() {
	if len(s.sseBuffer) == 0 {
		return
	}
	s.consumeFrame(s.sseBuffer)
	s.sseBuffer = nil
}

func (g *Gateway) logStreamEnd(
	ids requestIDs,
	model string,
	route modelRoute,
	external Protocol,
	obs *streamEndObserver,
	copyErr error,
	attempts int,
	clientCtx context.Context,
	requestCtx context.Context,
) {
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

	termination := streamTermination(obs, copyErr)
	completion := streamCompletion(obs)
	attrs := []any{
		"request_id", ids.Request,
		"model", model,
		"tier", string(route.Tier),
		"native_protocol", string(route.Protocol),
		"external_protocol", string(external),
		"outcome", outcome,
		"completion", completion,
		"termination", termination,
		"attempts", attempts,
		"has_content", obs.hasContent,
		"terminal_seen", obs.sawTerminal,
		"terminal_marker", obs.terminalMarker,
		"last_sse_event", obs.lastEventName,
		"last_event_kind", obs.lastEventKind,
		"reasoning_bytes", obs.reasoningBytes,
		"visible_bytes", obs.visibleBytes,
		"tool_bytes", obs.toolBytes,
		"chunks", obs.chunks,
		"sse_frames", obs.sseFrames,
		"parse_errors", obs.parseErrors,
		"bytes", obs.bytes,
		"max_gap_ms", obs.maxGap.Milliseconds(),
		"duration_ms", time.Since(obs.started).Milliseconds(),
	}
	if obs.readErr != nil {
		attrs = append(attrs, "read_error", obs.readErr.Error(), "read_error_class", streamErrorClass(obs.readErr))
	}
	if copyErr != nil {
		attrs = append(attrs, "copy_error", copyErr.Error(), "copy_error_class", streamErrorClass(copyErr))
	}
	if err := contextError(clientCtx); err != nil {
		attrs = append(attrs, "client_context_error", err.Error(), "client_context_error_class", streamErrorClass(err))
	}
	if err := contextError(requestCtx); err != nil {
		attrs = append(attrs, "request_context_error", err.Error(), "request_context_error_class", streamErrorClass(err))
	}
	g.logger.Log(context.Background(), slog.LevelInfo, "upstream stream ended", attrs...)
}
