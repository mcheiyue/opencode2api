package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrimIncompleteEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":"x\`, `{"a":"x`},
		{`{"a":"x\u`, `{"a":"x`},
		{`{"a":"x\u12`, `{"a":"x`},
		{`{"a":"x\\`, `{"a":"x\\`},
		{`{"a":"x\u1234`, `{"a":"x\u1234`},
		{`{"a":"plain`, `{"a":"plain`},
	}
	for _, c := range cases {
		if got := trimIncompleteEscape(c.in); got != c.want {
			t.Errorf("trimIncompleteEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRecoverToolArgsPrefix(t *testing.T) {
	if prefix, ok := recoverToolArgsPrefix(`{"file_path":"/tmp/m.md","content":"第一段`); !ok || prefix != "第一段" {
		t.Fatalf("open string value not recovered: %q ok=%v", prefix, ok)
	}
	if _, ok := recoverToolArgsPrefix(`{"file_path":"/tmp/m.md"`); ok {
		t.Fatal("a complete object must not be reported as recoverable")
	}
	if _, ok := recoverToolArgsPrefix(`{"a":{"b":"x`); ok {
		t.Fatal("a cut inside a nested object must not be recoverable")
	}
	if _, ok := recoverToolArgsPrefix(`{"a":12`); ok {
		t.Fatal("a cut inside a number must not be recoverable")
	}
}

func TestToolArgsShape(t *testing.T) {
	if closeObject, partial, ok := toolArgsShape(""); !ok || closeObject || partial != "" {
		t.Fatalf("empty raw must ask for the whole object: %v %q %v", closeObject, partial, ok)
	}
	if _, _, ok := toolArgsShape(`{"a":"x\u12`); ok {
		t.Fatal("a raw ending inside an escape sequence cannot be repaired downstream")
	}
	closeObject, partial, ok := toolArgsShape(`{"content":"第一段`)
	if !ok || !closeObject || partial != "第一段" {
		t.Fatalf("open string must continue and close: %v %q %v", closeObject, partial, ok)
	}
}

func TestEscapeJSONFragment(t *testing.T) {
	escaped, err := escapeJSONFragment("说 \"引号\" 和 \\ 反斜杠\n换行")
	if err != nil {
		t.Fatalf("escape failed: %v", err)
	}
	if _, err := json.Marshal(escaped); err != nil {
		t.Fatalf("escaped fragment is not a JSON string: %v", err)
	}
	unescaped, err := parseJSONString(`"` + escaped + `"`)
	if err != nil || unescaped != "说 \"引号\" 和 \\ 反斜杠\n换行" {
		t.Fatalf("round trip changed the fragment: %q %v", unescaped, err)
	}
}

func parseJSONString(quoted string) (string, error) {
	var value string
	err := json.Unmarshal([]byte(quoted), &value)
	return value, err
}

// toolArgsStreamResult runs one torn stream through the resumer and returns the
// client-visible bytes.
func toolArgsStreamResult(t *testing.T, body string, resumer *fakeResumer) (string, error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), recorder, strings.NewReader(body), Chat, "test-model", resumer, 1)
	return recorder.Body.String(), err
}

func TestForwardStreamResumable_splicesToolArgsIntoThePendingCall(t *testing.T) {
	resumer := &fakeResumer{toolArgs: true, toolSegment: func(request ToolArgsRequest) (io.Reader, error) {
		if request.Index != 0 || request.CallID != "call_1" {
			t.Errorf("continuation asked for the wrong call: %+v", request)
		}
		if request.Partial != "第一段文" {
			t.Errorf("recovered prefix = %q, want %q", request.Partial, "第一段文")
		}
		// The model answers with the remaining text, not with a tool frame.
		return strings.NewReader(contentFrame("第二段文") + doneFrame), nil
	}}
	body := toolCallFrameWith(0, "call_1", `{"content":"第一段文`)
	visible, _ := toolArgsStreamResult(t, body, resumer)

	if len(resumer.toolArgsCalls) != 1 {
		t.Fatalf("expected exactly one tool args continuation, got %d", len(resumer.toolArgsCalls))
	}
	assembled := splicedArguments(t, visible)
	if assembled != `{"content":"第一段文第二段文"}` {
		t.Fatalf("assembled arguments = %q", assembled)
	}
	if json.Unmarshal([]byte(assembled), &map[string]any{}) != nil {
		t.Fatalf("assembled arguments must parse: %q", assembled)
	}
	if !strings.Contains(visible, `"finish_reason":"tool_calls"`) || !strings.Contains(visible, "data: [DONE]") {
		t.Fatalf("expected a tool terminal, got %q", visible)
	}
	if strings.Contains(visible, "upstream_error") {
		t.Fatalf("a spliced tool call must not report an upstream error: %q", visible)
	}
	if !strings.Contains(visible, `"id":"call_1"`) {
		t.Fatalf("the pending call identity must be kept: %q", visible)
	}
}

// splicedArguments concatenates the argument fragments the client would append
// from the emitted frames, which is what the client's parser ends up with.
func splicedArguments(t *testing.T, visible string) string {
	t.Helper()
	var assembled strings.Builder
	for _, line := range strings.Split(visible, "\n") {
		payload := strings.TrimPrefix(line, "data: ")
		if payload == line || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		for _, choice := range frame.Choices {
			for _, call := range choice.Delta.ToolCalls {
				assembled.WriteString(call.Function.Arguments)
			}
		}
	}
	return assembled.String()
}

func escapeOrFail(t *testing.T, text string) string {
	t.Helper()
	escaped, err := escapeJSONFragment(text)
	if err != nil {
		t.Fatal(err)
	}
	return escaped
}

func TestForwardStreamResumable_asksForTheWholeObjectWhenNoArgumentArrived(t *testing.T) {
	resumer := &fakeResumer{toolArgs: true, toolSegment: func(request ToolArgsRequest) (io.Reader, error) {
		if request.Raw != "" || request.Partial != "" {
			t.Errorf("a header-only call must ask for the whole object: %+v", request)
		}
		if request.Name != "bash" {
			t.Errorf("tool name not carried: %+v", request)
		}
		return strings.NewReader(contentFrame(`{"cmd":"ls"}`) + doneFrame), nil
	}}
	// A tool call whose header arrived but whose arguments never did.
	body := chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "bash", "arguments": ""},
	}}})
	visible, _ := toolArgsStreamResult(t, body, resumer)

	if !strings.Contains(visible, `"finish_reason":"tool_calls"`) || !strings.Contains(visible, "data: [DONE]") {
		t.Fatalf("expected a tool terminal, got %q", visible)
	}
	if assembled := splicedArguments(t, visible); assembled != `{"cmd":"ls"}` {
		t.Fatalf("assembled arguments = %q", assembled)
	}
}

func TestForwardStreamResumable_refusesArgumentsThatDoNotParse(t *testing.T) {
	resumer := &fakeResumer{toolArgs: true, silent: true, toolSegment: func(ToolArgsRequest) (io.Reader, error) {
		return strings.NewReader(contentFrame("这不是 JSON") + doneFrame), nil
	}}
	body := chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "bash", "arguments": ""},
	}}})
	visible, _ := toolArgsStreamResult(t, body, resumer)

	if strings.Contains(visible, `"finish_reason":"tool_calls"`) {
		t.Fatalf("unparseable arguments must never get a tool terminal: %q", visible)
	}
}

func TestForwardStreamResumable_refusesAnEmptyContinuation(t *testing.T) {
	resumer := &fakeResumer{toolArgs: true, silent: true, toolSegment: func(ToolArgsRequest) (io.Reader, error) {
		// The model answered with a tool frame instead of the remaining text,
		// so the arguments would be closed short and still parse.
		return strings.NewReader(toolCallFrameWith(0, "call_1", "more") + doneFrame), nil
	}}
	body := toolCallFrameWith(0, "call_1", `{"content":"第一段文`)
	visible, _ := toolArgsStreamResult(t, body, resumer)

	if strings.Contains(visible, `"finish_reason":"tool_calls"`) {
		t.Fatalf("a continuation that delivered no text must not close the call: %q", visible)
	}
}

func TestForwardStreamResumable_toolArgsOffKeepsTheSkipPath(t *testing.T) {
	resumer := &fakeResumer{}
	body := toolCallFrameWith(0, "call_1", `{"content":"第一段文`)
	recorder := httptest.NewRecorder()
	if _, _, err := ForwardStreamResumable(context.Background(), recorder, strings.NewReader(body), Chat, "test-model", resumer, 1); err == nil {
		t.Fatal("with the feature off the turn must still fail")
	}
	if len(resumer.toolArgsCalls) != 0 {
		t.Fatal("no tool args continuation may be fetched when the feature is off")
	}
	if len(resumer.skipped) == 0 || !strings.HasPrefix(resumer.skipped[0], "tool_seen") {
		t.Fatalf("skip reason changed: %v", resumer.skipped)
	}
}

func TestForwardStreamResumable_toolArgsRefusesMultipleCalls(t *testing.T) {
	resumer := &fakeResumer{toolArgs: true}
	first := toolCallFrameWith(0, "call_1", `{"cmd":"ls"}`)
	second := toolCallFrameWith(1, "call_2", `{"cmd":"pwd`)
	recorder := httptest.NewRecorder()
	if _, _, err := ForwardStreamResumable(context.Background(), recorder, strings.NewReader(first+second), Chat, "test-model", resumer, 1); err == nil {
		t.Fatal("two tool calls cannot be spliced onto one pending call")
	}
	if len(resumer.toolArgsCalls) != 0 {
		t.Fatal("a multi-call stream must not fetch a tool args continuation")
	}
	if len(resumer.skipped) == 0 || resumer.skipped[0] != "tool_args_multi_call" {
		t.Fatalf("expected the multi-call refusal to be recorded first: %v", resumer.skipped)
	}
}
