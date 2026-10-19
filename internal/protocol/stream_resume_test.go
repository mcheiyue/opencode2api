package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func chatFrame(delta map[string]any) string {
	payload := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(encoded) + "\n\n"
}

func roleFrame() string {
	return chatFrame(map[string]any{"role": "assistant", "content": ""})
}

func contentFrame(content string) string {
	return chatFrame(map[string]any{"content": content})
}

func toolCallFrame() string {
	return chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "bash", "arguments": `{"cmd`},
	}}})
}

func toolCallFrameWith(index int, id, arguments string) string {
	return chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": id, "type": "function",
		"function": map[string]any{"name": "bash", "arguments": arguments},
	}}})
}

func completeToolCallFrame() string {
	return toolCallFrameWith(0, "call_1", `{"cmd":"ls"}`)
}

const doneFrame = "data: [DONE]\n\n"

const tornFragment = `data: {"id":"chatcmpl-test","choices":[{"delta":{"content":"wor`

type fakeResumer struct {
	calls         []string
	reasonCalls   []string
	skipped       []string
	visibleBytes  int
	reasonBytes   int
	toolSeen      bool
	silent        bool
	toolArgs      bool
	toolArgsCalls []ToolArgsRequest
	toolSegment   func(request ToolArgsRequest) (io.Reader, error)
	segment       func(partial string, reasoning string) (io.Reader, error)
}

func (f *fakeResumer) SilentClose() bool { return f.silent }

func (f *fakeResumer) ToolArgsEnabled() bool { return f.toolArgs }

func (f *fakeResumer) ResumeToolArgs(request ToolArgsRequest) (io.Reader, error) {
	f.toolArgsCalls = append(f.toolArgsCalls, request)
	if f.toolSegment == nil {
		return nil, errors.New("no tool args segment configured")
	}
	return f.toolSegment(request)
}

func (f *fakeResumer) ResumeSegment(partial string, reasoning string) (io.Reader, error) {
	f.calls = append(f.calls, partial)
	f.reasonCalls = append(f.reasonCalls, reasoning)
	if f.segment != nil {
		return f.segment(partial, reasoning)
	}
	return nil, errors.New("no continuation segment configured")
}

func (f *fakeResumer) ResumeEligible(visibleBytes, reasoningBytes int, toolSeen bool) {
	f.visibleBytes = visibleBytes
	f.reasonBytes = reasoningBytes
	f.toolSeen = toolSeen
}

func (f *fakeResumer) ResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	f.skipped = append(f.skipped, reason)
	f.visibleBytes = visibleBytes
	f.reasonBytes = reasoningBytes
	f.toolSeen = toolSeen
}

var createdPattern = regexp.MustCompile(`"created":\d+`)

func normalizeCreated(body string) string {
	return createdPattern.ReplaceAllString(body, `"created":0`)
}

func frameJSON(t *testing.T, frame string) map[string]any {
	t.Helper()
	data, ok := sseFrameData([]byte(frame))
	if !ok {
		t.Fatalf("frame has no data payload: %q", frame)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("unmarshal frame data: %v", err)
	}
	return payload
}

func deltaContent(t *testing.T, payload map[string]any) string {
	t.Helper()
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("frame has no choices: %v", payload)
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	content, _ := delta["content"].(string)
	return content
}

func TestForwardStreamResumable_normalStreamMatchesForwardStream(t *testing.T) {
	stream := roleFrame() + contentFrame("Hello") + contentFrame(" world") + doneFrame

	baseline := httptest.NewRecorder()
	if _, _, err := ForwardStream(context.Background(), baseline, strings.NewReader(stream), Chat, "test-model"); err != nil {
		t.Fatalf("ForwardStream: %v", err)
	}

	resumer := &fakeResumer{}
	resumed := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), resumed, strings.NewReader(stream), Chat, "test-model", resumer, 1)
	if err != nil {
		t.Fatalf("ForwardStreamResumable: %v", err)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer called %d times on a normal stream, want 0", len(resumer.calls))
	}
	got, want := normalizeCreated(resumed.Body.String()), normalizeCreated(baseline.Body.String())
	if got != want {
		t.Fatalf("resumable output differs from ForwardStream output\nwant: %q\ngot:  %q", want, got)
	}
}

func TestForwardStreamResumable_tornStreamResumesOnce(t *testing.T) {
	torn := roleFrame() + contentFrame("Hello") + tornFragment
	segment2 := contentFrame(" world") + doneFrame
	resumer := &fakeResumer{segment: func(partial string, reasoning string) (io.Reader, error) {
		return strings.NewReader(segment2), nil
	}}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err != nil {
		t.Fatalf("expected nil error after successful resume, got %v", err)
	}
	if len(resumer.calls) != 1 || resumer.calls[0] != "Hello" {
		t.Fatalf("resumer calls = %q, want [Hello]", resumer.calls)
	}
	out := rec.Body.String()
	if strings.Contains(out, "upstream_error") || strings.Contains(out, "upstream SSE stream ended") {
		t.Fatalf("output contains an in-band error frame: %q", out)
	}
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("output lost the first segment text: %q", out)
	}
	if !strings.Contains(out, `"content":" world"`) {
		t.Fatalf("output lost the continuation text: %q", out)
	}
	if !strings.HasSuffix(out, doneFrame) {
		t.Fatalf("output does not end with [DONE]: %q", out)
	}
	// The torn fragment must not be glued to the continuation's first frame.
	if !strings.Contains(out, tornFragment+"\n\n") {
		t.Fatalf("torn fragment was not terminated before the continuation: %q", out)
	}
}

func TestForwardStreamResumable_tornTailTerminatedWhenNoTrailingNewline(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
		wantGap  string
	}{
		{name: "no newline", fragment: `data: {"choices":[{"delta":{"content":"wor`, wantGap: "\n\n"},
		{name: "one newline", fragment: "data: {\"choices\":[{\"delta\":{\"content\":\"wor\n", wantGap: "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			torn := roleFrame() + contentFrame("Hello") + tc.fragment
			segment2 := contentFrame(" world") + doneFrame
			resumer := &fakeResumer{segment: func(partial string, reasoning string) (io.Reader, error) {
				return strings.NewReader(segment2), nil
			}}
			rec := httptest.NewRecorder()
			_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
			if err != nil {
				t.Fatalf("expected nil error after successful resume, got %v", err)
			}
			out := rec.Body.String()
			if !strings.Contains(out, tc.fragment+tc.wantGap+"data: ") {
				t.Fatalf("torn tail was not separated from the next segment (want %q after fragment): %q", tc.wantGap, out)
			}
		})
	}
}

func TestForwardStreamResumable_staleParseErrorDoesNotFailCleanResume(t *testing.T) {
	torn := contentFrame("Hello") + "data: {not-json}\n\n" + tornFragment
	segment2 := contentFrame(" world") + doneFrame
	resumer := &fakeResumer{segment: func(partial string, reasoning string) (io.Reader, error) {
		return strings.NewReader(segment2), nil
	}}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err != nil {
		t.Fatalf("expected nil error after a clean resume, got %v", err)
	}
	if len(resumer.calls) != 1 {
		t.Fatalf("resumer calls = %d, want 1", len(resumer.calls))
	}
	out := rec.Body.String()
	if strings.Contains(out, "upstream_error") {
		t.Fatalf("stale parse error emitted an in-band error frame: %q", out)
	}
	if !strings.HasSuffix(out, doneFrame) {
		t.Fatalf("output does not end with [DONE]: %q", out)
	}
}

func TestForwardStreamResumable_toolDeltaBlocksResume(t *testing.T) {
	torn := roleFrame() + toolCallFrame() + tornFragment
	resumer := &fakeResumer{segment: func(partial string, reasoning string) (io.Reader, error) {
		return strings.NewReader(contentFrame("late") + doneFrame), nil
	}}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err == nil {
		t.Fatal("expected an error for a stream torn inside a tool call")
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer called %d times after a tool delta, want 0", len(resumer.calls))
	}
	if len(resumer.skipped) != 1 || !strings.HasPrefix(resumer.skipped[0], "tool_seen[") || !resumer.toolSeen {
		t.Fatalf("resume skip telemetry = reasons:%v tool_seen:%t", resumer.skipped, resumer.toolSeen)
	}
	if !strings.Contains(resumer.skipped[0], "calls=1") || !strings.Contains(resumer.skipped[0], "parse=") {
		t.Fatalf("skip reason must carry the tool state, got %q", resumer.skipped[0])
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("expected an in-band error frame, got %q", rec.Body.String())
	}
}

func TestForwardStreamResumable_silentCloseEndsTurnWithoutErrorFrame(t *testing.T) {
	torn := roleFrame() + contentFrame("Hello wor") + tornFragment
	resumer := &fakeResumer{silent: true}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err == nil || !strings.Contains(err.Error(), "closed silently") {
		t.Fatalf("a silently closed turn must still report the failure to telemetry, got %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected a synthetic terminal chunk, got %q", body)
	}
	if strings.Contains(body, "upstream_error") {
		t.Fatalf("silent close must not write an in-band error frame: %q", body)
	}
}

func TestForwardStreamResumable_secondTornSegmentEmitsError(t *testing.T) {
	torn := roleFrame() + contentFrame("Hello") + tornFragment
	segment2 := contentFrame(" more") + tornFragment
	resumer := &fakeResumer{segment: func(partial string, reasoning string) (io.Reader, error) {
		return strings.NewReader(segment2), nil
	}}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err == nil {
		t.Fatal("expected an error when the continuation segment is also torn")
	}
	if len(resumer.calls) != 1 {
		t.Fatalf("resumer calls = %d, want 1 (resume budget exhausted)", len(resumer.calls))
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("expected an in-band error frame, got %q", rec.Body.String())
	}
}

func TestForwardStreamResumable_noVisibleTextSkipsResume(t *testing.T) {
	torn := roleFrame() + `data: {"id":"chatcmpl-test","choices":[{"delta":`
	resumer := &fakeResumer{}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err == nil {
		t.Fatal("expected an error for a stream torn before any visible text")
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer called %d times without visible text, want 0", len(resumer.calls))
	}
	if len(resumer.skipped) != 1 || resumer.skipped[0] != "no_visible_text" {
		t.Fatalf("resume skip telemetry = %v, want [no_visible_text]", resumer.skipped)
	}
}

func TestForwardStreamResumable_reasoningOnlyTriggersReasoningResume(t *testing.T) {
	torn := chatFrame(map[string]any{"reasoning_content": "private reasoning"}) + tornFragment
	resumer := &fakeResumer{
		segment: func(partial string, reasoning string) (io.Reader, error) {
			if partial != "" {
				t.Fatalf("partial = %q, want empty for reasoning-only stream", partial)
			}
			if reasoning != "private reasoning" {
				t.Fatalf("reasoning = %q, want 'private reasoning'", reasoning)
			}
			return strings.NewReader(contentFrame("resumed answer") + doneFrame), nil
		},
	}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err != nil {
		t.Fatalf("ForwardStreamResumable with reasoning resume failed: %v", err)
	}
	if len(resumer.calls) != 1 || resumer.calls[0] != "" {
		t.Fatalf("resumer calls = %v, want 1 call with empty partial", resumer.calls)
	}
	if len(resumer.reasonCalls) != 1 || resumer.reasonCalls[0] != "private reasoning" {
		t.Fatalf("resumer reasonCalls = %v, want ['private reasoning']", resumer.reasonCalls)
	}
	if !strings.Contains(rec.Body.String(), "resumed answer") {
		t.Fatalf("expected resumed answer in downstream stream, got %q", rec.Body.String())
	}
}

func TestForwardStreamResumable_clientCanceledSkipsResumeAndErrorFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	torn := roleFrame() + contentFrame("Hello") + tornFragment
	resumer := &fakeResumer{}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(ctx, rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer called %d times after client cancel, want 0", len(resumer.calls))
	}
	if len(resumer.skipped) != 1 || resumer.skipped[0] != "client_canceled" {
		t.Fatalf("resume skip telemetry = %v, want [client_canceled]", resumer.skipped)
	}
	if strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("canceled client received an error frame: %q", rec.Body.String())
	}
}

func TestLCPTrimReader_overlapFiveTrimmed(t *testing.T) {
	src := strings.NewReader(contentFrame("ABCDEF") + doneFrame)
	out, err := io.ReadAll(newLCPTrimReader(src, "ABCDE"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"content":"ABCDEF"`) {
		t.Fatalf("overlap was not trimmed: %q", got)
	}
	if !strings.Contains(got, `"content":"F"`) {
		t.Fatalf("trimmed content missing: %q", got)
	}
	if !strings.HasSuffix(got, doneFrame) {
		t.Fatalf("terminator lost: %q", got)
	}
}

func TestLCPTrimReader_overlapBelowMinimumUntouched(t *testing.T) {
	src := strings.NewReader(contentFrame("XYZAB") + doneFrame)
	out, err := io.ReadAll(newLCPTrimReader(src, "XYZ"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"content":"XYZAB"`) {
		t.Fatalf("sub-minimum overlap must not trim: %q", got)
	}
}

func TestLCPTrimReader_invalidJSONPassthrough(t *testing.T) {
	src := "data: {not-json}\n\n" + contentFrame("Hello") + doneFrame
	out, err := io.ReadAll(newLCPTrimReader(strings.NewReader(src), "tail"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != src {
		t.Fatalf("invalid JSON must pass through unchanged\nwant: %q\ngot:  %q", src, string(out))
	}
}

func TestLCPTrimReader_doneFramePassthrough(t *testing.T) {
	src := doneFrame + contentFrame("later")
	out, err := io.ReadAll(newLCPTrimReader(strings.NewReader(src), "tail"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != src {
		t.Fatalf("[DONE] must pass through unchanged\nwant: %q\ngot:  %q", src, string(out))
	}
}

type chunkReader struct {
	data []byte
	size int
}

func (reader *chunkReader) Read(p []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	n := min(reader.size, len(p), len(reader.data))
	copy(p, reader.data[:n])
	reader.data = reader.data[n:]
	return n, nil
}

func TestLCPTrimReader_contentFrameSplitAcrossReads(t *testing.T) {
	raw := contentFrame("ABCDEF") + doneFrame
	out, err := io.ReadAll(newLCPTrimReader(&chunkReader{data: []byte(raw), size: 7}, "ABCDE"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"content":"F"`) {
		t.Fatalf("split frame was not trimmed: %q", got)
	}
	if !strings.HasSuffix(got, doneFrame) {
		t.Fatalf("terminator lost after split reads: %q", got)
	}
}

func TestLCPTrimReader_multibyteRunesSplitAcrossReads(t *testing.T) {
	raw := contentFrame("你好世界啊") + doneFrame
	out, err := io.ReadAll(newLCPTrimReader(&chunkReader{data: []byte(raw), size: 3}, "你好世界"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"content":"你好世界啊"`) {
		t.Fatalf("multi-byte overlap was not trimmed: %q", got)
	}
	if !strings.Contains(got, `"content":"啊"`) {
		t.Fatalf("multi-byte trimmed content missing: %q", got)
	}
	if !strings.HasSuffix(got, doneFrame) {
		t.Fatalf("terminator lost after multi-byte split: %q", got)
	}
}

func TestLongestOverlap(t *testing.T) {
	cases := []struct {
		name string
		text string
		tail string
		maxK int
		want int
	}{
		{name: "exact five", text: "world peace", tail: "hello world", maxK: 200, want: 5},
		{name: "suffix full match", text: "bcdef", tail: "abcdef", maxK: 200, want: 5},
		{name: "minimum four", text: "cdef", tail: "abcdef", maxK: 200, want: 4},
		{name: "below minimum", text: "xyz", tail: "abcxyz", maxK: 200, want: 0},
		{name: "identical short", text: "aaaa", tail: "aaaa", maxK: 200, want: 4},
		{name: "multibyte runes", text: "世界和平年代", tail: "你好世界和平", maxK: 200, want: 4},
		{name: "maxK cap", text: strings.Repeat("a", 300), tail: strings.Repeat("a", 300), maxK: 200, want: 200},
		{name: "empty text", text: "", tail: "abc", maxK: 200, want: 0},
		{name: "empty tail", text: "abc", tail: "", maxK: 200, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := longestOverlap(tc.text, tc.tail, tc.maxK); got != tc.want {
				t.Fatalf("longestOverlap(%q, %q, %d) = %d, want %d", tc.text, tc.tail, tc.maxK, got, tc.want)
			}
		})
	}
}

func TestObserverTextAccumulation(t *testing.T) {
	observer := streamUsageObserver(Chat)
	observer.trackContent = true
	frame := roleFrame() + contentFrame("Hello")
	if _, err := observer.Write([]byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	observer.Finish()
	if observer.Text() != "Hello" {
		t.Fatalf("Text() = %q, want Hello", observer.Text())
	}
	if observer.TextRunes() != 5 {
		t.Fatalf("TextRunes() = %d, want 5", observer.TextRunes())
	}
	if observer.ToolSeen() {
		t.Fatal("ToolSeen() = true without tool frames")
	}
	if got := observer.TailRunes(3); got != "llo" {
		t.Fatalf("TailRunes(3) = %q, want llo", got)
	}
}

func TestObserverTrackContentDisabledSkipsCopies(t *testing.T) {
	observer := streamUsageObserver(Chat)
	frame := roleFrame() + contentFrame("Hello")
	if _, err := observer.Write([]byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	observer.Finish()
	if observer.Text() != "" {
		t.Fatalf("Text() = %q, want empty when trackContent is off", observer.Text())
	}
	if observer.visibleBytes != 5 {
		t.Fatalf("visibleBytes = %d, want 5 (bytes still counted)", observer.visibleBytes)
	}
}

func TestLCPTrimReader_crossFrameOverlapTrimmed(t *testing.T) {
	// Real Chat streams emit ~1 token per frame; a 5-rune overlap is split
	// across frames, so single-frame LCP would never fire.
	src := contentFrame("ABCDE") + contentFrame("FG") + doneFrame
	out, err := io.ReadAll(newLCPTrimReader(strings.NewReader(src), "ABCDE"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"content":"ABCDE"`) {
		t.Fatalf("cross-frame overlap head not trimmed: %q", got)
	}
	if !strings.Contains(got, `"content":"FG"`) {
		t.Fatalf("surviving content missing: %q", got)
	}
	if !strings.HasSuffix(got, doneFrame) {
		t.Fatalf("terminator lost: %q", got)
	}
}

func TestLCPTrimReader_crossFrameMultiByteOverlapTrimmed(t *testing.T) {
	src := contentFrame("你好") + contentFrame("世界啊") + doneFrame
	out, err := io.ReadAll(newLCPTrimReader(strings.NewReader(src), "你好世界"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"content":"你好"`) {
		t.Fatalf("multi-byte cross-frame head not trimmed: %q", got)
	}
	if !strings.Contains(got, `"content":"啊"`) {
		t.Fatalf("multi-byte surviving content missing: %q", got)
	}
}

func TestContinuationFrameFilter_dropsRoleAndReasoning(t *testing.T) {
	reasoningFrame := chatFrame(map[string]any{"role": "assistant", "content": "", "reasoning": "thinking..."})
	src := roleFrame() + reasoningFrame + contentFrame("Hello") + doneFrame
	out, err := io.ReadAll(newContinuationFrameFilter(strings.NewReader(src)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "thinking...") {
		t.Fatalf("reasoning frame leaked through filter: %q", got)
	}
	if strings.Contains(got, `"content":""`) {
		t.Fatalf("role head frame leaked through filter: %q", got)
	}
	if !strings.Contains(got, `"content":"Hello"`) {
		t.Fatalf("content frame dropped by filter: %q", got)
	}
	if !strings.HasSuffix(got, doneFrame) {
		t.Fatalf("terminator lost: %q", got)
	}
}

func TestContinuationFrameFilter_keepsFinishAndUsage(t *testing.T) {
	finish := `data: {"id":"chatcmpl-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n"
	src := contentFrame("Hi") + finish + doneFrame
	out, err := io.ReadAll(newContinuationFrameFilter(strings.NewReader(src)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("finish frame dropped: %q", got)
	}
	if !strings.Contains(got, `"total_tokens":15`) {
		t.Fatalf("usage frame dropped: %q", got)
	}
}

func TestObserverCarriesUpstreamErrorDetail(t *testing.T) {
	observer := streamUsageObserver(Chat)
	observer.trackContent = true
	errorFrame := `data: {"error":{"message":"free tier rate limited","type":"rate_limit_error"}}` + "\n\n"
	if _, err := observer.Write([]byte(errorFrame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !observer.ErrorTermination() {
		t.Fatal("error frame must terminate the stream as an error")
	}
	got := observer.UpstreamError()
	if !errors.Is(got, errStreamUpstreamFailure) {
		t.Fatalf("detail must wrap the sentinel so errors.Is still matches: %v", got)
	}
	if !strings.Contains(got.Error(), "free tier rate limited") || !strings.Contains(got.Error(), "rate_limit_error") {
		t.Fatalf("upstream type/message not carried into the error: %v", got)
	}
}

func TestObserverUpstreamErrorDefaultsToSentinel(t *testing.T) {
	observer := streamUsageObserver(Chat)
	if got := observer.UpstreamError(); got.Error() != errStreamUpstreamFailure.Error() {
		t.Fatalf("observer without an error frame must return the bare sentinel: %v", got)
	}
}

func TestObserverCompleteToolCalls(t *testing.T) {
	complete := streamUsageObserver(Chat)
	if _, err := complete.Write([]byte(roleFrame() + completeToolCallFrame())); err != nil {
		t.Fatalf("write: %v", err)
	}
	complete.Finish()
	if !complete.CompleteToolCalls() {
		t.Fatal("CompleteToolCalls() = false for a fully delivered tool call")
	}

	partial := streamUsageObserver(Chat)
	if _, err := partial.Write([]byte(roleFrame() + toolCallFrame())); err != nil {
		t.Fatalf("write: %v", err)
	}
	partial.Finish()
	if partial.CompleteToolCalls() {
		t.Fatal("CompleteToolCalls() = true for arguments cut mid-JSON")
	}

	namelessFrame := chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "function": map[string]any{"arguments": `{"cmd":"ls"}`},
	}}})
	nameless := streamUsageObserver(Chat)
	if _, err := nameless.Write([]byte(roleFrame() + namelessFrame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	nameless.Finish()
	if nameless.CompleteToolCalls() {
		t.Fatal("CompleteToolCalls() = true for a tool call without a name")
	}

	none := streamUsageObserver(Chat)
	if _, err := none.Write([]byte(roleFrame() + contentFrame("hello"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	none.Finish()
	if none.CompleteToolCalls() {
		t.Fatal("CompleteToolCalls() = true without any tool call")
	}
}

func TestForwardStreamResumable_salvagesCompleteToolCall(t *testing.T) {
	torn := roleFrame() + completeToolCallFrame() + tornFragment
	resumer := &fakeResumer{}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err != nil {
		t.Fatalf("a fully delivered tool call must not be reported as a failure: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream_error") {
		t.Fatalf("expected no in-band error frame, got %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing salvaged tool terminal: %q", body)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer called %d times, want 0", len(resumer.calls))
	}
	if len(resumer.skipped) != 0 {
		t.Fatalf("a salvaged turn must not be logged as skipped, got %v", resumer.skipped)
	}
}

func TestForwardStreamResumable_partialSiblingToolCallStillFails(t *testing.T) {
	torn := roleFrame() +
		completeToolCallFrame() +
		toolCallFrameWith(1, "call_2", `{"cmd":"ls`) +
		tornFragment
	resumer := &fakeResumer{}
	rec := httptest.NewRecorder()
	_, _, err := ForwardStreamResumable(context.Background(), rec, strings.NewReader(torn), Chat, "test-model", resumer, 1)
	if err == nil {
		t.Fatal("one complete tool call must not mask a sibling cut mid-arguments")
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("expected an in-band error frame, got %q", rec.Body.String())
	}
}

func TestObserverToolState(t *testing.T) {
	complete := streamUsageObserver(Chat)
	if _, err := complete.Write([]byte(roleFrame() + completeToolCallFrame())); err != nil {
		t.Fatalf("write: %v", err)
	}
	complete.Finish()
	if state := complete.ToolState(); state != "calls=1 named=1 argbytes=12 parse=ok" {
		t.Fatalf("ToolState() = %q for a complete call", state)
	}

	truncated := streamUsageObserver(Chat)
	if _, err := truncated.Write([]byte(roleFrame() + toolCallFrame())); err != nil {
		t.Fatalf("write: %v", err)
	}
	truncated.Finish()
	if state := truncated.ToolState(); !strings.Contains(state, "calls=1") || strings.Contains(state, "parse=ok") {
		t.Fatalf("ToolState() = %q for arguments cut mid-JSON", state)
	}

	// A delta whose call carries neither index nor id resolves to no key, so
	// nothing is accumulated even though the observer did see a tool delta.
	keyless := chatFrame(map[string]any{"tool_calls": []any{map[string]any{
		"type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`},
	}}})
	unkeyed := streamUsageObserver(Chat)
	if _, err := unkeyed.Write([]byte(roleFrame() + keyless)); err != nil {
		t.Fatalf("write: %v", err)
	}
	unkeyed.Finish()
	if !unkeyed.ToolSeen() {
		t.Fatal("a keyless tool delta must still count as a seen tool call")
	}
	if state := unkeyed.ToolState(); state != "calls=0" {
		t.Fatalf("ToolState() = %q for a keyless tool delta", state)
	}
	if unkeyed.CompleteToolCalls() {
		t.Fatal("a keyless tool delta must never be treated as complete")
	}
}
