package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// continuationInstruction is the empirically verified user message appended
// after the delivered partial; the free tier has no assistant prefill, so a
// bare trailing assistant message regenerates from scratch instead of
// continuing.
const continuationInstruction = "Continue exactly where you stopped. Do not repeat anything already written."

const reasoningContinuationInstruction = "You have already completed the following detailed reasoning process:\n\n<thinking>\n%s\n</thinking>\n\nBased on your reasoning above, do not repeat or re-evaluate the thinking. Output your final answer directly now."

const maxContinuationPartialBytes = 512 << 10

const toolArgsFullInstruction = "Your previous response was cut off before you finished calling the tool %q. Call that tool now: output only the arguments object for it, as a single JSON object starting with { and ending with }, with no explanation, no markdown fences and no other text."

const toolArgsContinueInstruction = "以下是正在写入的文档已完成的部分，它在中间被截断了：\n\n<<<PARTIAL>>>\n%s\n<<<END>>>\n\n请从中断处原样继续输出剩余原文。只输出剩余部分：不要重复已有内容、不要任何解释、不要 JSON 或代码块包装。"

var errContinuationTimeout = errors.New("stream continuation fetch timed out")

type gatewayStreamResumer struct {
	g                   *Gateway
	clientCtx           context.Context
	streamState         *anonymousStreamState
	continuationTimeout time.Duration
	attemptTimeout      time.Duration
	silentClose         bool
	toolArgs            bool
	route               models.Route
	ids                 identity.RequestIDs
	zenBody             []byte
	meta                *telemetry.RequestMeta
	attemptOffset       int
}

// ResumeSegment builds the continuation body from the delivered partial,
// cools down the exit that just broke so the pool cursor rotates onto a
// different one, and fetches the next segment on a client-bound budget.
func (r *gatewayStreamResumer) ResumeSegment(partial string, reasoning string) (io.Reader, error) {
	if partial == "" && reasoning == "" {
		r.recordResumeFailure("empty_partial")
		return nil, errors.New("stream continuation requires delivered text or reasoning")
	}
	if len(partial) > maxContinuationPartialBytes {
		r.recordResumeFailure("partial_too_large")
		return nil, fmt.Errorf("stream continuation partial of %d bytes exceeds the %d byte limit", len(partial), maxContinuationPartialBytes)
	}
	if len(reasoning) > maxContinuationPartialBytes {
		reasoning = reasoning[len(reasoning)-maxContinuationPartialBytes:]
	}
	if err := r.readyForContinuation(len(partial), len(reasoning), false); err != nil {
		return nil, err
	}
	requestBody, err := buildContinuationBody(r.zenBody, partial, reasoning)
	if err != nil {
		r.recordResumeFailure("invalid_continuation_body")
		return nil, err
	}
	kind := "reasoning"
	if partial != "" {
		kind = "text"
	}
	return r.fetchContinuation(requestBody, kind, len(partial), len(reasoning))
}

// ResumeToolArgs fetches the rest of a tool call's arguments so the call the
// client already holds can be finished instead of dropped.
func (r *gatewayStreamResumer) ResumeToolArgs(request wire.ToolArgsRequest) (io.Reader, error) {
	if !r.toolArgs {
		r.recordResumeSkipped("tool_args_disabled", 0, 0, true)
		return nil, errors.New("tool arguments continuation is disabled")
	}
	if err := r.readyForContinuation(0, 0, true); err != nil {
		return nil, err
	}
	requestBody, err := buildToolArgsBody(r.zenBody, request)
	if err != nil {
		r.recordResumeFailure("invalid_continuation_body")
		return nil, err
	}
	kind := "tool_args_full"
	if request.Partial != "" {
		kind = "tool_args_continue"
	}
	return r.fetchContinuation(requestBody, kind, len(request.Partial), 0)
}

// readyForContinuation holds the guards every continuation shares: a live
// client context and a positive budget.
func (r *gatewayStreamResumer) readyForContinuation(partialBytes, reasoningBytes int, toolSeen bool) error {
	if r.clientCtx == nil {
		r.recordResumeFailure("missing_client_context")
		return errors.New("stream continuation requires a client context")
	}
	if err := r.clientCtx.Err(); err != nil {
		r.recordResumeSkipped("client_canceled", partialBytes, reasoningBytes, toolSeen)
		return err
	}
	if r.continuationTimeout <= 0 {
		r.recordResumeSkipped("budget_disabled", partialBytes, reasoningBytes, toolSeen)
		return errors.New("stream continuation budget is disabled")
	}
	return nil
}

func (r *gatewayStreamResumer) fetchContinuation(requestBody []byte, kind string, partialBytes, reasoningBytes int) (io.Reader, error) {
	continuationCtx, cancel := context.WithCancelCause(r.clientCtx)
	continuationCtx = context.WithValue(continuationCtx, anonymousStreamStateKey{}, r.streamState)
	continuationCtx = withContinuationAttemptTimeout(continuationCtx, r.attemptTimeout)
	var timeoutMu sync.Mutex
	timeoutCommitted := false
	timer := time.AfterFunc(r.continuationTimeout, func() {
		timeoutMu.Lock()
		defer timeoutMu.Unlock()
		if !timeoutCommitted {
			cancel(errContinuationTimeout)
		}
	})
	keepContext := false
	defer func() {
		timer.Stop()
		if !keepContext {
			cancel(nil)
		}
	}()
	r.g.markCurrentStreamExitFailed(continuationCtx)
	bodies := map[config.Tier][]byte{config.TierZen: requestBody}
	if r.meta != nil {
		r.meta.ResumeAttempts++
		r.meta.ResumeKind = kind
	}
	if r.g.logger != nil {
		r.g.logger.Info("stream continuation attempt dispatched", "component", "upstream", "event", "stream_continuation_attempted", "request_id", r.ids.Request, "model", r.route.ID, "partial_bytes", partialBytes, "reasoning_bytes", reasoningBytes, "resume_kind", kind, "timeout_ms", r.continuationTimeout.Milliseconds(), "attempt_timeout_ms", r.attemptTimeout.Milliseconds())
	}
	resp, err, attempts := r.g.doAnonymousUpstream(continuationCtx, r.route, bodies, r.ids, r.attemptOffset)
	r.attemptOffset += attempts
	timeoutMu.Lock()
	cause := context.Cause(continuationCtx)
	if cause == nil {
		timeoutCommitted = true
		timer.Stop()
	}
	timeoutMu.Unlock()
	if cause != nil {
		if resp != nil && resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		resp = nil
		err = cause
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		reason := "upstream_error"
		if errors.Is(err, errContinuationTimeout) {
			reason = "continuation_timeout"
		} else if r.clientCtx.Err() != nil {
			reason = "client_canceled"
		}
		r.recordResumeFailure(reason)
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		status := resp.StatusCode
		if resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		r.recordResumeFailure("upstream_http_error")
		return nil, fmt.Errorf("stream continuation upstream returned status %d", status)
	}
	if resp.Body == nil {
		r.recordResumeFailure("upstream_empty_body")
		return nil, errors.New("stream continuation upstream returned no body")
	}
	continued := 0
	if r.meta != nil {
		r.meta.Continued++
		r.meta.ResumeSuccesses++
		r.meta.Legs = append(r.meta.Legs, "cont:resume")
		continued = r.meta.Continued
	}
	keepContext = true
	if r.g.logger != nil {
		r.g.logger.Info("stream continued on a new exit", "component", "upstream", "event", "stream_continued", "request_id", r.ids.Request, "model", r.route.ID, "partial_bytes", partialBytes, "reasoning_bytes", reasoningBytes, "resume_kind", kind, "resume_attempt", continued)
	}
	return &continuationBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

type continuationBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (body *continuationBody) Close() error {
	body.cancel(nil)
	return body.ReadCloser.Close()
}

func (r *gatewayStreamResumer) ToolArgsEnabled() bool { return r.toolArgs }

func (r *gatewayStreamResumer) SilentClose() bool { return r.silentClose }

func (r *gatewayStreamResumer) ResumeEligible(visibleBytes, reasoningBytes int, toolSeen bool) {
	if r.meta != nil {
		r.meta.ResumeVisibleBytes = visibleBytes
		r.meta.ResumeReasoningBytes = reasoningBytes
		r.meta.ResumeToolSeen = toolSeen
		if visibleBytes == 0 && reasoningBytes > 0 {
			r.meta.ResumeKind = "reasoning"
		} else {
			r.meta.ResumeKind = "text"
		}
	}
}

func (r *gatewayStreamResumer) ResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	if r.meta != nil {
		if r.meta.ResumeSkipped == "" {
			r.meta.ResumeSkipped = reason
		}
		r.meta.ResumeVisibleBytes = visibleBytes
		r.meta.ResumeReasoningBytes = reasoningBytes
		r.meta.ResumeToolSeen = toolSeen
	}
	if r.g != nil && r.g.logger != nil {
		r.g.logger.Debug("stream continuation skipped", "component", "upstream", "event", "stream_continuation_skipped", "request_id", r.ids.Request, "model", r.route.ID, "reason", reason, "visible_bytes", visibleBytes, "reasoning_bytes", reasoningBytes, "tool_seen", toolSeen)
	}
}

func (r *gatewayStreamResumer) recordResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	r.ResumeSkipped(reason, visibleBytes, reasoningBytes, toolSeen)
}

func (r *gatewayStreamResumer) recordResumeFailure(reason string) {
	if r.meta != nil {
		r.meta.ResumeFailures++
	}
	if r.g != nil && r.g.logger != nil {
		r.g.logger.Warn("stream continuation failed", "component", "upstream", "event", "stream_continuation_failed", "request_id", r.ids.Request, "model", r.route.ID, "reason", reason)
	}
}

// buildContinuationBody appends the delivered partial plus the continuation
// instruction to the original Zen body. A trailing assistant message is
// merged in place so the request never carries two adjacent assistant turns;
// every other field (tools, stream options, effort, ...) is preserved. The
// anonymous free-tier shape is NOT re-asserted here: doAnonymousUpstream
// already runs prepareAnonymousBody on every body it sends, so shaping here
// as well was a redundant second pass (review: "prepareAnonymousBody called
// twice").
func buildContinuationBody(zenBody []byte, partial string, reasoning string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(zenBody, &payload); err != nil {
		return nil, fmt.Errorf("parse continuation base body: %w", err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("continuation base body has no messages")
	}
	if partial != "" {
		if last, ok := messages[len(messages)-1].(map[string]any); ok && last["role"] == "assistant" {
			// Multimodal turns carry content-part arrays; flatten the text
			// parts so the partial can still merge (review: array content
			// broke construction).
			last["content"] = flattenMessageContent(last["content"]) + partial
		} else {
			messages = append(messages, map[string]any{"role": "assistant", "content": partial})
		}
		messages = append(messages, map[string]any{"role": "user", "content": continuationInstruction})
	} else if reasoning != "" {
		instruction := "\n\n" + fmt.Sprintf(reasoningContinuationInstruction, reasoning)
		if last, ok := messages[len(messages)-1].(map[string]any); ok && last["role"] == "user" {
			if content, ok := last["content"].(string); ok {
				last["content"] = content + instruction
			} else {
				messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf(reasoningContinuationInstruction, reasoning)})
			}
		} else {
			messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf(reasoningContinuationInstruction, reasoning)})
		}
	}
	payload["messages"] = messages
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("continuation body contains unsupported JSON values")
	}
	return encoded, nil
}

// buildToolArgsBody appends the instruction that makes the model finish the
// arguments it was still writing; the recovered prefix is inlined when there is
// one, because the free tier has no assistant prefill to continue from.
func buildToolArgsBody(zenBody []byte, request wire.ToolArgsRequest) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(zenBody, &payload); err != nil {
		return nil, fmt.Errorf("parse tool arguments base body: %w", err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("tool arguments base body has no messages")
	}
	instruction := fmt.Sprintf(toolArgsFullInstruction, request.Name)
	if request.Partial != "" {
		instruction = fmt.Sprintf(toolArgsContinueInstruction, request.Partial)
	}
	payload["messages"] = append(messages, map[string]any{"role": "user", "content": instruction})
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("tool arguments body contains unsupported JSON values")
	}
	return encoded, nil
}

// flattenMessageContent renders a Chat message content value (string or
// content-part array) as plain text.
func flattenMessageContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var out strings.Builder
		for _, part := range value {
			if entry, ok := part.(map[string]any); ok {
				if text, ok := entry["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	default:
		return ""
	}
}

// streamResumeLimit returns the number of resumes allowed after the first
// break; values below 1 mean one resume, matching the shipped default.
func (g *Gateway) streamResumeLimit() int {
	limit := g.cfg.Performance.StreamContinuationMax
	if limit < 1 {
		limit = 1
	}
	return limit
}

func attemptOffsetFromMeta(meta *telemetry.RequestMeta) int {
	if meta == nil {
		return 0
	}
	return meta.Attempts
}
