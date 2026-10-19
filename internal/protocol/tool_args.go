package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// errToolArgsUnspliceable marks a continuation that must not be handed to the
// client: either it cannot be attached to the pending tool call, or the
// assembled arguments would not parse. The caller closes the turn without a
// tool terminal in that case, because a half written file is worse than a
// truncation.
var errToolArgsUnspliceable = errors.New("tool arguments continuation cannot be spliced")

// trimIncompleteEscape drops a trailing escape sequence that was cut in half so
// the remaining bytes end on a character boundary inside the string value.
func trimIncompleteEscape(raw string) string {
	end := len(raw)
	for end > 0 && raw[end-1] == '\\' {
		end--
	}
	if (len(raw)-end)%2 == 1 {
		return raw[:len(raw)-1]
	}
	if escape := strings.LastIndex(raw, `\u`); escape >= 0 {
		digits := raw[escape+2:]
		if len(digits) < 4 && isHexDigits(digits) {
			return raw[:escape]
		}
	}
	return raw
}

func isHexDigits(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// recoverToolArgsPrefix returns the decoded prefix of the top-level string value
// the model was still writing. ok is false when the cut landed anywhere else,
// which text continuation cannot extend.
func recoverToolArgsPrefix(raw string) (string, bool) {
	trimmed := trimIncompleteEscape(raw)
	probe := map[string]any{}
	if json.Unmarshal([]byte(trimmed+`"}`), &probe) != nil {
		return "", false
	}
	for _, value := range probe {
		text, isString := value.(string)
		if !isString || text == "" {
			continue
		}
		encoded, err := json.Marshal(text)
		if err != nil || len(encoded) < 2 {
			continue
		}
		if strings.HasSuffix(trimmed, string(encoded[1:len(encoded)-1])) {
			return text, true
		}
	}
	return "", false
}

// toolArgsShape classifies a pending tool call's arguments. closeObject is set
// when the stream died inside a string value, so the spliced text continues it
// and the object still needs closing; it is clear when no argument byte arrived
// and the model has to produce the whole arguments object.
func toolArgsShape(raw string) (closeObject bool, partial string, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return false, "", true
	}
	// The client already holds the delivered bytes, so a raw that ends inside
	// an escape sequence cannot be repaired from here.
	if trimIncompleteEscape(raw) != raw {
		return false, "", false
	}
	partial, ok = recoverToolArgsPrefix(raw)
	if !ok {
		return false, "", false
	}
	return true, partial, true
}

// escapeJSONFragment renders text as the inside of a JSON string so it can be
// appended to a tool call's arguments without breaking the object.
func escapeJSONFragment(text string) (string, error) {
	encoded, err := json.Marshal(text)
	if err != nil {
		return "", fmt.Errorf("escape tool arguments fragment: %w", err)
	}
	if len(encoded) < 2 {
		return "", errors.New("escape tool arguments fragment: empty encoding")
	}
	return string(encoded[1 : len(encoded)-1]), nil
}

// toolArgsSpliceReader rewrites one continuation segment into argument deltas
// for the tool call the client already holds, then closes the argument object
// and terminates the turn.
type toolArgsSpliceReader struct {
	source      io.Reader
	probe       *streamUsageTracker
	responseID  string
	model       string
	request     ToolArgsRequest
	closeObject bool
	delivered   int
	out         []byte
	done        bool
	sawContent  bool
	rejected    bool
	sourceErr   error
}

func newToolArgsSpliceReader(source io.Reader, responseID, model string, request ToolArgsRequest, closeObject bool) io.Reader {
	probe := streamUsageObserver(Chat)
	probe.trackContent = true
	splice := &toolArgsSpliceReader{
		probe:       probe,
		responseID:  responseID,
		model:       model,
		request:     request,
		closeObject: closeObject,
	}
	// The probe sees the continuation's own frames so only its visible text is
	// spliced; reasoning frames must not become file content.
	splice.source = io.TeeReader(source, probe)
	return splice
}

func (reader *toolArgsSpliceReader) Read(p []byte) (int, error) {
	for {
		if len(reader.out) > 0 {
			n := copy(p, reader.out)
			reader.out = reader.out[n:]
			return n, nil
		}
		if reader.done {
			return 0, io.EOF
		}
		if reader.rejected {
			reader.done = true
			return 0, errToolArgsUnspliceable
		}
		if reader.sourceErr != nil {
			if err := reader.finish(); err != nil {
				reader.done = true
				return 0, err
			}
			reader.done = true
			continue
		}
		buf := make([]byte, 16<<10)
		_, err := reader.source.Read(buf)
		if err != nil {
			reader.sourceErr = err
		}
		if emitErr := reader.emitNew(); emitErr != nil {
			reader.rejected = true
		}
	}
}

// emitNew appends argument deltas for the segment text that arrived since the
// last call.
func (reader *toolArgsSpliceReader) emitNew() error {
	text := reader.probe.Text()
	if len(text) <= reader.delivered {
		return nil
	}
	fresh := text[reader.delivered:]
	reader.delivered = len(text)
	if head := strings.TrimSpace(fresh); head != "" {
		if reader.closeObject && !reader.sawContent && (strings.HasPrefix(head, "{") || strings.Contains(fresh, "```")) {
			return fmt.Errorf("%w: continuation wrapped the text in JSON or markdown", errToolArgsUnspliceable)
		}
		reader.sawContent = true
	}
	if reader.closeObject {
		return reader.appendFragment(fresh)
	}
	return reader.emitFragment(fresh)
}

func (reader *toolArgsSpliceReader) appendFragment(fragment string) error {
	escaped, err := escapeJSONFragment(fragment)
	if err != nil {
		return err
	}
	return reader.emitFragment(escaped)
}

// emitFragment emits an already escaped fragment: the closing bytes of the
// argument object are JSON syntax and must reach the client unescaped, and in
// whole-object mode the model's own text is that syntax.
func (reader *toolArgsSpliceReader) emitFragment(escaped string) error {
	frame, err := toolArgsDeltaFrame(reader.responseID, reader.model, reader.request.Index, reader.request.CallID, escaped)
	if err != nil {
		return err
	}
	reader.out = append(reader.out, frame...)
	return nil
}

// finish validates the assembled arguments before terminating: a tool terminal
// is only emitted for arguments that parse, so the client never executes a
// half written value.
func (reader *toolArgsSpliceReader) finish() error {
	if reader.closeObject && reader.delivered == 0 {
		// The segment carried no continuation text, so the arguments would be
		// closed short of what the model was writing and still parse.
		return fmt.Errorf("%w: no continuation text was delivered", errToolArgsUnspliceable)
	}
	assembled := reader.request.Raw + reader.probe.Text()
	if reader.closeObject {
		assembled += `"}`
	}
	if json.Unmarshal([]byte(assembled), &map[string]any{}) != nil {
		return fmt.Errorf("%w: assembled arguments did not parse", errToolArgsUnspliceable)
	}
	if reader.closeObject {
		if err := reader.emitFragment(`"}`); err != nil {
			return err
		}
	}
	reader.out = append(reader.out, syntheticToolFinishFrames(reader.responseID, reader.model)...)
	return nil
}

func toolArgsDeltaFrame(responseID, model string, index int, callID, escapedFragment string) ([]byte, error) {
	payload := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    index,
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"arguments": escapedFragment},
			}}},
			"finish_reason": nil,
		}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode tool arguments delta: %w", err)
	}
	return []byte("data: " + string(encoded) + "\n\n"), nil
}

func syntheticToolFinishFrames(responseID, model string) []byte {
	payload := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte("data: [DONE]\n\n")
	}
	return append(append([]byte("data: "), encoded...), []byte("\n\ndata: [DONE]\n\n")...)
}
