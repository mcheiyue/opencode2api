package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestStreamEndObserverRecordsTerminalAndCounters(t *testing.T) {
	body := strings.NewReader(
		`data: {"id":"resp-1","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
			`data: {"id":"resp-1","choices":[{"delta":{"reasoning_content":"think"}}]}` + "\n\n" +
			`data: {"id":"resp-1","choices":[{"delta":{"content":"answer"}}]}` + "\n\n" +
			`data: {"id":"resp-1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	observer := newStreamEndObserver(body, ProtocolChat)
	if _, err := io.Copy(io.Discard, observer); err != nil {
		t.Fatalf("copy stream: %v", err)
	}
	observer.finish()

	if !observer.hasContent || !observer.sawTerminal {
		t.Fatalf("content/terminal = %t/%t, want true/true", observer.hasContent, observer.sawTerminal)
	}
	if observer.terminalMarker != "done" {
		t.Fatalf("terminal marker = %q, want done", observer.terminalMarker)
	}
	if observer.reasoningBytes != 5 || observer.visibleBytes != 6 {
		t.Fatalf("content counters = reasoning %d visible %d, want 5/6", observer.reasoningBytes, observer.visibleBytes)
	}
	if observer.sseFrames != 5 || observer.parseErrors != 0 {
		t.Fatalf("SSE counters = frames %d parse_errors %d, want 5/0", observer.sseFrames, observer.parseErrors)
	}
	if observer.lastEventKind != "done" {
		t.Fatalf("last event kind = %q, want done", observer.lastEventKind)
	}
}

func TestStreamEndObserverFlushesUnterminatedFinalEvent(t *testing.T) {
	body := strings.NewReader(`data: {"id":"resp-1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n")
	observer := newStreamEndObserver(body, ProtocolChat)
	if _, err := io.Copy(io.Discard, observer); err != nil {
		t.Fatalf("copy stream: %v", err)
	}
	observer.finish()

	if !observer.sawTerminal {
		t.Fatal("terminal marker was not found in an unterminated final SSE event")
	}
	if observer.terminalMarker != "finish:stop" {
		t.Fatalf("terminal marker = %q, want finish:stop", observer.terminalMarker)
	}
}

func TestStreamTerminationUnexpectedEOFAfterContent(t *testing.T) {
	observer := &streamEndObserver{readErr: io.ErrUnexpectedEOF, hasContent: true}
	got := streamTermination(observer, nil)
	if got != "upstream_unexpected_eof" {
		t.Fatalf("termination = %q, want upstream_unexpected_eof", got)
	}
}

func TestStreamTerminationClientContextIsRecordedSeparately(t *testing.T) {
	clientCtx, cancel := context.WithCancel(context.Background())
	cancel()
	observer := &streamEndObserver{readErr: context.Canceled}
	if got := streamTermination(observer, nil); got != "read_context_canceled" {
		t.Fatalf("termination = %q, want read_context_canceled", got)
	}
	if err := contextError(clientCtx); err != context.Canceled {
		t.Fatalf("client context error = %v, want context canceled", err)
	}
}