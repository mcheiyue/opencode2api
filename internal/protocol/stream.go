package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

var errStreamNormalTermination = errors.New("upstream stream terminated normally")

var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

type bridgeStreamEvent struct {
	Kind       string
	ResponseID string
	Model      string
	Text       string
	Signature  string
	ToolKey    string
	ToolID     string
	ToolName   string
	Stop       string
	Error      string
	ErrorType  string
	Encrypted  string
	Usage      *Usage
}

// TranscodeStream is the request-aware form used by the
// gateway. A cancelled client must not receive a synthetic upstream error
// after its connection has gone away.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	termination := streamOpen
	readErr := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			termination = streamErrorTermination
			if emitErr := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: err.Error(), ErrorType: "upstream_error"}); emitErr != nil {
				return emitErr
			}
			return errStreamUpstreamFailure
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				termination = streamNormalTermination
			case "error":
				termination = streamErrorTermination
			}
			if err := emitter.Emit(event); err != nil {
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, nil
		}
		if ClientCanceled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, readErr
		}
		if termination == streamOpen {
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, emitErr
			}
		}
		return emitter.usage, emitter.usageReported, readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, err
	}
	return emitter.usage, emitter.usageReported, errSSEUnexpectedEOF
}

func emitUnexpectedStreamError(emitter *bridgeStreamEmitter, cause error) error {
	message := "upstream SSE stream ended before a terminal event"
	if cause != nil && !errors.Is(cause, errSSEUnexpectedEOF) {
		message = fmt.Sprintf("upstream SSE stream failed: %v", cause)
	}
	err := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: message, ErrorType: "upstream_error"})
	if errors.Is(err, errStreamUpstreamFailure) {
		return nil
	}
	return err
}

// emitSalvagedToolFinish closes a Chat stream that died after the upstream had
// delivered every tool call in full. The client holds valid tool_call frames
// but never the terminal chunk, so without this it reports a failed turn for a
// tool step that is actually complete. Only Chat is handled: the lane that
// truncates this way is Chat-only, and the other protocols' terminals need the
// per-block bookkeeping this observer does not keep.
func emitSalvagedToolFinish(w io.Writer, flusher http.Flusher, protocol Protocol, model, responseID string) error {
	return emitSyntheticFinish(w, flusher, protocol, model, responseID, "tool_calls")
}

// emitSilentFinish closes a turn the client cannot receive in full with the
// same terminal a complete turn would send, so the truncation never reaches the
// client as an error; the caller keeps reporting the failure internally.
func emitSilentFinish(w io.Writer, flusher http.Flusher, protocol Protocol, model, responseID string) error {
	return emitSyntheticFinish(w, flusher, protocol, model, responseID, "stop")
}

func emitSyntheticFinish(w io.Writer, flusher http.Flusher, protocol Protocol, model, responseID, finishReason string) error {
	if protocol != Chat {
		return fmt.Errorf("no synthetic terminal for %s stream", protocol)
	}
	if responseID == "" {
		responseID = identity.RandomID("chatcmpl", 12)
	}
	emitter := newBridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitter.sse("", map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": emitter.created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	}); err != nil {
		return err
	}
	return emitter.rawSSE("", "[DONE]")
}

func ClientCanceled(ctx context.Context, streamErr error) bool {
	if errors.Is(streamErr, context.Canceled) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// sseFlushWriter preserves an upstream SSE byte stream while making each
// successful write visible to the client immediately. io.Copy is free to
// choose large writes, so flushing in Write is the only reliable place to
// keep same-protocol streams live.
type sseFlushWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (writer *sseFlushWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if n > 0 {
		writer.flusher.Flush()
	}
	return n, err
}

// StreamResumer fetches a replacement upstream SSE segment after the
// previous one died mid-flight. partial is the visible text already
// delivered downstream; reasoning is the thinking text delivered downstream
// when the stream died before any visible text was produced.
type StreamResumer interface {
	ResumeSegment(partial string, reasoning string) (io.Reader, error)
}

type StreamResumeObserver interface {
	ResumeEligible(visibleBytes, reasoningBytes int, toolSeen bool)
	ResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool)
}

// SilentCloser is an optional resumer capability: when it reports true, a turn
// that cannot be delivered in full ends with an ordinary terminal instead of an
// in-band error frame.
type SilentCloser interface {
	SilentClose() bool
}

// ToolArgsRequest is a tool call whose arguments were still arriving when the
// upstream stream died. Partial is the recovered prefix of the string value the
// model was writing; it is empty when no argument byte arrived at all.
type ToolArgsRequest struct {
	Index   int
	CallID  string
	Name    string
	Raw     string
	Partial string
}

// ToolArgsResumer is an optional resumer capability: fetch the rest of a tool
// call's arguments on another exit.
type ToolArgsResumer interface {
	ToolArgsEnabled() bool
	ResumeToolArgs(req ToolArgsRequest) (io.Reader, error)
}

// ForwardStream relays one upstream SSE stream to the client with no resume
// support; it is the thin wrapper that pins the historical behavior.
func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	return ForwardStreamResumable(ctx, w, reader, protocol, model, nil, 0)
}

// RenameStreamReader wraps an SSE stream reader and rewrites tool call names
// matching fromName back to toName before downstream parsers or writers consume
// the stream. When fromName or toName is empty (or identical), reader is
// returned unchanged.
//
// It is a pull-based transform instead of a goroutine plus io.Pipe so a client
// that disconnects mid-stream cannot strand a blocked writer.
func RenameStreamReader(reader io.Reader, protocol Protocol, fromName, toName string) io.Reader {
	if fromName == "" || toName == "" || fromName == toName {
		return reader
	}
	return &renameStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		renamer: &streamToolRenamer{protocol: protocol, fromName: fromName, toName: toName, tools: map[string]*streamToolRenameState{}},
	}
}

type renameStreamReader struct {
	buffer  *bufio.Reader
	renamer *streamToolRenamer
	pending []byte
	err     error
}

func (reader *renameStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		// ReadString either returns a non-empty chunk with a nil error or
		// returns the trailing chunk together with EOF, so this loop can only
		// repeat while a rewritten chunk is still being drained.
		line, err := reader.buffer.ReadString('\n')
		if len(line) > 0 {
			reader.pending = reader.rewriteLine(line)
		}
		if err != nil {
			// The final chunk is drained before the error is reported so a
			// truncated stream still delivers every byte the upstream sent.
			reader.err = err
			if len(reader.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// rewriteLine transforms one complete SSE line, preserving its original bytes
// and terminator whenever nothing needs to be rewritten.
func (reader *renameStreamReader) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" || raw == "[DONE]" {
		return []byte(line)
	}
	rewritten := reader.renamer.rewriteFrame(raw)
	if string(rewritten) == raw {
		return []byte(line)
	}
	return []byte("data: " + string(rewritten) + terminator)
}

// streamToolRenameState tracks how one tool call's name was resolved while a
// stream is rewritten. Chat deltas may repeat a growing name prefix or append
// fragments, so a value that is still a prefix of fromName is held back until
// it either completes the match or diverges.
type streamToolRenameState struct {
	decided bool
	renamed bool
	pending string
}

type streamToolRenamer struct {
	protocol Protocol
	fromName string
	toName   string
	tools    map[string]*streamToolRenameState
}

// rewriteFrame rewrites one decoded SSE data object.
func (renamer *streamToolRenamer) rewriteFrame(raw string) []byte {
	if renamer.protocol != Chat {
		// Anthropic and Responses streams always carry a complete tool name in
		// a single event, so the stateless object rewrite is exact.
		if !strings.Contains(raw, renamer.fromName) {
			return []byte(raw)
		}
		return RenameResponseTool([]byte(raw), renamer.protocol, renamer.fromName, renamer.toName)
	}
	if !strings.Contains(raw, `"tool_calls"`) {
		return []byte(raw)
	}
	return renamer.rewriteChatFrame(raw)
}

func (renamer *streamToolRenamer) rewriteChatFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	changed := false
	for _, rawChoice := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := rawChoice.(map[string]any)
		for _, container := range []string{"delta", "message"} {
			message, _ := choice[container].(map[string]any)
			if message == nil {
				continue
			}
			for callIndex, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if function == nil {
					continue
				}
				state := renamer.toolState(fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id"), callIndex)))
				if name, ok := function["name"].(string); ok && name != "" {
					if renamer.applyNameFragment(state, function, name) {
						changed = true
					}
				}
				if _, ok := function["arguments"]; ok && !state.decided {
					// Arguments can only start once the name is complete: flush
					// whatever was held back, verbatim.
					if state.pending != "" {
						function["name"] = state.pending
						changed = true
					}
					state.decided = true
				}
			}
		}
	}
	if !changed {
		return []byte(raw)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(raw)
	}
	return encoded
}

// applyNameFragment folds one name fragment into a tool's rename state and
// reports whether the frame changed.
func (renamer *streamToolRenamer) applyNameFragment(state *streamToolRenameState, function map[string]any, fragment string) bool {
	if state.decided {
		if !state.renamed {
			return false
		}
		// A resolved alias accepts no further fragments: any continuation
		// belongs to a longer name the client never declared.
		if _, exists := function["name"]; exists {
			delete(function, "name")
			return true
		}
		return false
	}
	state.pending = mergeToolName(state.pending, fragment)
	switch {
	case state.pending == renamer.fromName:
		function["name"] = renamer.toName
		state.decided, state.renamed = true, true
		return true
	case strings.HasPrefix(renamer.fromName, state.pending):
		// Still a prefix of the alias: hold the fragment until a later frame
		// either completes the match or diverges.
		delete(function, "name")
		return true
	default:
		function["name"] = state.pending
		state.decided = true
		return true
	}
}

func (renamer *streamToolRenamer) toolState(key string) *streamToolRenameState {
	if state := renamer.tools[key]; state != nil {
		return state
	}
	state := &streamToolRenameState{}
	renamer.tools[key] = state
	return state
}

type streamUsageObserver struct {
	parser   *bridgeStreamParser
	buffer   []byte
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
	// text accumulates the visible deltas already delivered downstream. A
	// mid-stream resume needs exactly this text to build the continuation
	// request, so it is capped at 1 MiB to bound per-request memory.
	// trackContent gates both builders: without a mounted resumer the
	// disabled path must not pay any copy cost (review: "disabled path
	// not free").
	text         strings.Builder
	reasoning    strings.Builder
	trackContent bool
	toolSeen     bool
	// trackTools accumulates each tool call's name and argument fragments so a
	// stream that dies after a complete tool call can be closed with a real
	// terminal instead of an error. Only Chat streams truncate this way (the
	// continuation lane is Chat-only), so other protocols pay nothing.
	trackTools     bool
	toolNames      map[string]string
	toolIDs        map[string]string
	toolArgs       map[string]*strings.Builder
	toolOrder      []string
	responseID     string
	visibleBytes   int
	reasoningBytes int
	errorType      string
	errorMessage   string
}

const maxErrorDetailBytes = 1024

// maxToolArgumentsBytes bounds one tool call's accumulated arguments; a larger
// object cannot parse and is treated as incomplete.
const maxToolArgumentsBytes = 1 << 20

func newStreamUsageObserver(protocol Protocol) *streamUsageObserver {
	return &streamUsageObserver{
		parser: &bridgeStreamParser{
			protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
			responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
		},
		trackTools: protocol == Chat,
		toolNames:  map[string]string{},
		toolIDs:    map[string]string{},
		toolArgs:   map[string]*strings.Builder{},
	}
}

func (observer *streamUsageObserver) Write(data []byte) (int, error) {
	observer.buffer = append(observer.buffer, data...)
	for {
		index, width := nextSSEBoundary(observer.buffer)
		if index < 0 {
			break
		}
		frame := append([]byte(nil), observer.buffer[:index+width]...)
		observer.buffer = observer.buffer[index+width:]
		observer.consume(frame)
	}
	return len(data), nil
}

func (observer *streamUsageObserver) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageObserver) Reported() bool { return observer.reported }

func (observer *streamUsageObserver) NormalTermination() bool { return observer.normal }

func (observer *streamUsageObserver) ErrorTermination() bool { return observer.error }

func (observer *streamUsageObserver) ParseError() error { return observer.parseErr }

// UpstreamError returns the error reported when the upstream delivered its
// own error frame, wrapping errStreamUpstreamFailure so errors.Is still
// matches while the gateway logs the real upstream type and message.
func (observer *streamUsageObserver) UpstreamError() error {
	if observer.errorType == "" && observer.errorMessage == "" {
		return errStreamUpstreamFailure
	}
	return fmt.Errorf("%w: type=%s message=%s", errStreamUpstreamFailure, observer.errorType, observer.errorMessage)
}

// Text returns the visible text delivered downstream so far, capped at 1 MiB.
func (observer *streamUsageObserver) Text() string { return observer.text.String() }

// TextRunes reports the delivered visible text length in runes; resume
// eligibility keys on this, not on bytes.
func (observer *streamUsageObserver) TextRunes() int {
	return utf8.RuneCountInString(observer.text.String())
}

func (observer *streamUsageObserver) Reasoning() string { return observer.reasoning.String() }

func (observer *streamUsageObserver) ReasoningRunes() int {
	return utf8.RuneCountInString(observer.reasoning.String())
}

// ToolSeen reports whether any tool call delta appeared; a stream broken
// inside a tool call must never be resumed as plain text.
func (observer *streamUsageObserver) ToolSeen() bool { return observer.toolSeen }

// PendingToolCall reports the one tool call whose arguments never completed.
// ok is false when the stream carried no call, more than one, or a call whose
// key is not a usable index: argument fragments can only be spliced back onto a
// single unambiguously identified call.
func (observer *streamUsageObserver) PendingToolCall() (ToolArgsRequest, bool) {
	if !observer.toolSeen || len(observer.toolOrder) != 1 {
		return ToolArgsRequest{}, false
	}
	key := observer.toolOrder[0]
	index, err := strconv.Atoi(key)
	if err != nil || index < 0 {
		return ToolArgsRequest{}, false
	}
	request := ToolArgsRequest{Index: index, CallID: observer.toolIDs[key], Name: observer.toolNames[key]}
	if builder := observer.toolArgs[key]; builder != nil {
		request.Raw = builder.String()
	}
	return request, true
}

// ResponseID is the id the upstream announced for this stream, so a synthetic
// terminal chunk carries the same id as the frames it closes.
func (observer *streamUsageObserver) ResponseID() string { return observer.responseID }

// noteTool registers a tool call in stream order and returns its argument
// builder, or nil when the event carries no usable key.
func (observer *streamUsageObserver) noteTool(key, name string) *strings.Builder {
	if key == "" {
		return nil
	}
	builder, seen := observer.toolArgs[key]
	if !seen {
		builder = &strings.Builder{}
		observer.toolArgs[key] = builder
		observer.toolOrder = append(observer.toolOrder, key)
	}
	if name != "" {
		observer.toolNames[key] = name
	}
	return builder
}

// CompleteToolCalls reports whether every tool call seen on the stream has a
// name and an argument object that parses. The upstream marks the end of a
// call's arguments only in the terminal chunk, which a truncated stream never
// sent, so a parseable object is the only evidence a call is whole; arguments
// cut mid-JSON fail to parse and are rejected.
func (observer *streamUsageObserver) CompleteToolCalls() bool {
	if len(observer.toolOrder) == 0 {
		return false
	}
	for _, key := range observer.toolOrder {
		if observer.toolNames[key] == "" {
			return false
		}
		builder := observer.toolArgs[key]
		if builder == nil || builder.Len() == 0 {
			return false
		}
		var arguments map[string]any
		if err := json.Unmarshal([]byte(builder.String()), &arguments); err != nil {
			return false
		}
	}
	return true
}

// ToolState renders a compact diagnostic of the accumulated tool calls so a
// refused salvage can be told apart from a stream that died mid-arguments;
// calls=0 means a tool delta arrived under an unresolvable key.
func (observer *streamUsageObserver) ToolState() string {
	if len(observer.toolOrder) == 0 {
		return "calls=0"
	}
	named, argBytes := 0, 0
	parseErr := ""
	for _, key := range observer.toolOrder {
		if observer.toolNames[key] != "" {
			named++
		}
		builder := observer.toolArgs[key]
		if builder == nil {
			continue
		}
		argBytes += builder.Len()
		if parseErr == "" {
			var arguments map[string]any
			if err := json.Unmarshal([]byte(builder.String()), &arguments); err != nil {
				parseErr = err.Error()
			}
		}
	}
	state := fmt.Sprintf("calls=%d named=%d argbytes=%d", len(observer.toolOrder), named, argBytes)
	if parseErr != "" {
		return state + " parse=" + parseErr
	}
	return state + " parse=ok"
}

// TailRunes returns the last n runes of the delivered text, rune-safe, for
// the LCP overlap window of a continuation segment.
func (observer *streamUsageObserver) TailRunes(n int) string {
	runes := []rune(observer.text.String())
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[len(runes)-n:])
}

// Pending returns a copy of the bytes delivered after the most recent frame
// boundary. A stream that died mid-frame left its torn frame here; the
// resume splice must terminate it before appending a new segment, or the
// next frame joins the torn event and is swallowed by the client parser.
// Must be called before Finish, which clears the buffer.
func (observer *streamUsageObserver) Pending() []byte {
	if len(observer.buffer) == 0 {
		return nil
	}
	return append([]byte(nil), observer.buffer...)
}

func (observer *streamUsageObserver) consume(frame []byte) {
	_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		events, err := observer.parser.Parse(eventName, data)
		if err != nil {
			if observer.parseErr == nil {
				observer.parseErr = err
			}
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				observer.normal = true
			case "start":
				if observer.responseID == "" {
					observer.responseID = event.ResponseID
				}
			case "error":
				observer.error = true
				if observer.errorMessage == "" && observer.errorType == "" {
					observer.errorType = event.ErrorType
					observer.errorMessage = event.Error
					if len(observer.errorMessage) > maxErrorDetailBytes {
						observer.errorMessage = observer.errorMessage[:maxErrorDetailBytes]
					}
				}
			case "text":
				observer.visibleBytes += len(event.Text)
				// Stop appending once the cap is reached; the continuation
				// body guard rejects partials this large anyway.
				if observer.trackContent && event.Text != "" && observer.text.Len() < 1<<20 {
					observer.text.WriteString(event.Text)
				}
			case "reasoning":
				observer.reasoningBytes += len(event.Text) + len(event.Encrypted)
				if observer.trackContent && event.Text != "" && observer.reasoning.Len() < 512<<10 {
					observer.reasoning.WriteString(event.Text)
				}
			case "tool_start", "tool_delta":
				observer.toolSeen = true
				if observer.trackTools {
					if event.ToolID != "" {
						observer.toolIDs[event.ToolKey] = event.ToolID
					}
					builder := observer.noteTool(event.ToolKey, event.ToolName)
					if event.Kind == "tool_delta" && builder != nil && event.Text != "" && builder.Len() < maxToolArgumentsBytes {
						builder.WriteString(event.Text)
					}
				}
			}
			if event.Usage != nil {
				observer.reported = true
				mergeBridgeUsage(&observer.usage, *event.Usage)
			}
		}
		return nil
	})
}

func nextSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var eventName string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 {
		// A blank line is the SSE record delimiter. Do not parse a final
		// unterminated record as a complete frame; an EOF without a terminal
		// event is handled by the caller as a truncated stream.
		return errSSEUnexpectedEOF
	}
	return nil
}
