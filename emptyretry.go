package main

// ponytail: empty-response detection for transparent retry.
// Reads upstream SSE frames into a buffer until a decision is reached:
//   - probeContentFound: at least one non-empty content/reasoning/tool delta
//   - probeEmptyResponse: stream ended (EOF or zero content
//   - probePrematureClose: stream ended without terminal marker but had content
//
// Ceiling: 64KB buffer. If upstream sends >64KB without a meaningful delta
// (extremely unlikely), we treat it as contentFound to avoid unbounded buffering.
// Upgrade path: per-key budget instead of global constant.

import (
	"io"
	"strings"
)

type probeResult int

const (
	probeContentFound probeResult = iota
	probeEmptyResponse
	probePrematureClose
)

func (r probeResult) String() string {
	switch r {
	case probeContentFound:
		return "content_found"
	case probeEmptyResponse:
		return "empty_response"
	case probePrematureClose:
		return "premature_close"
	default:
		return "unknown"
	}
}

// probeStreamContent reads from reader until it can determine whether the
// stream contains meaningful content. Buffered bytes are written to *buffered
// for replay. Returns the probe result.
func probeStreamContent(reader io.Reader, protocol Protocol, buffered *[]byte, maxBuffer int) probeResult {
	parser := &bridgeStreamParser{
		protocol:          protocol,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	var sseBuf []byte
	hasContent := false
	sawTerminal := false
	buf := make([]byte, 8192)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			*buffered = append(*buffered, chunk...)
			sseBuf = append(sseBuf, chunk...)
			// Process complete SSE frames
			for {
				index, width := nextSSEBoundary(sseBuf)
				if index < 0 {
					break
				}
				frame := append([]byte(nil), sseBuf[:index+width]...)
				sseBuf = sseBuf[index+width:]
				_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
					events, perr := parser.Parse(eventName, data)
					if perr != nil {
						return nil
					}
					for _, event := range events {
						switch event.Kind {
						case "text", "reasoning", "tool_delta":
							if event.Text != "" {
								hasContent = true
							}
						case "done", "finish":
							sawTerminal = true
						}
					}
					return nil
				})
			}
			// Decision: if we found content, we're done probing
			if hasContent {
				return probeContentFound
			}
			// Buffer cap: if we've buffered too much without content, pass through
			if len(*buffered) >= maxBuffer {
				return probeContentFound
			}
		}
		if err != nil {
			// Stream ended — process any remaining buffered frame
			if len(sseBuf) > 0 {
				_ = readSSE(strings.NewReader(string(sseBuf)), func(eventName, data string) error {
					events, perr := parser.Parse(eventName, data)
					if perr != nil {
						return nil
					}
					for _, event := range events {
						switch event.Kind {
						case "text", "reasoning", "tool_delta":
							if event.Text != "" {
								hasContent = true
							}
						case "done", "finish":
							sawTerminal = true
						}
					}
					return nil
				})
			}
			if hasContent && !sawTerminal {
				return probePrematureClose
			}
			if hasContent {
				return probeContentFound
			}
			return probeEmptyResponse
		}
	}
}
