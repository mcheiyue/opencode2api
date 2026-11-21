package httpx

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newBody(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}

// RoundTrip replays a request on a fresh connection only when repeating it
// cannot duplicate a side effect: safe methods, or a caller that promised
// duplicate suppression with an idempotency key. The buffered body is a full
// copy, so a missing GetBody must not block the opt-in replay.
func TestBunReplayableRespectsMethodAndBodySafety(t *testing.T) {
	body := []byte(`{"model":"gpt-5"}`)
	cases := []struct {
		name     string
		method   string
		withBody bool
		key      string
		want     bool
	}{
		{name: "get", method: http.MethodGet, want: true},
		{name: "empty method", method: "", want: true},
		{name: "head", method: http.MethodHead, want: true},
		{name: "post without body or key", method: http.MethodPost, want: false},
		{name: "post with key and buffered body", method: http.MethodPost, withBody: true, key: "abc", want: true},
		{name: "patch with key", method: http.MethodPatch, withBody: true, key: "abc", want: true},
		{name: "delete without key", method: http.MethodDelete, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(test.method, "https://example.invalid/v1/chat/completions", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if test.withBody {
				req.Body = newBody(body)
			}
			if test.key != "" {
				req.Header.Set("Idempotency-Key", test.key)
			}
			if got := bunReplayable(req); got != test.want {
				t.Errorf("bunReplayable(%s, key=%q) = %v, want %v", test.method, test.key, got, test.want)
			}
		})
	}
}

// A request head must reject a field that could split the request line, so a
// replayed request cannot smuggle a second credential header onto the wire.
func TestBunRequestHeadRejectsUnsafeFields(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v1/chat/completions", newBody([]byte("{}")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	head, _, err := bunRequestHead(req, []byte("{}"))
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if got := strings.Count(string(head), "Authorization:"); got != 1 {
		t.Errorf("Authorization appears %d times in the request head, want 1:\n%s", got, head)
	}

	req.Header.Set("X-Evil", "value\r\nAuthorization: Bearer other")
	if _, _, err := bunRequestHead(req, []byte("{}")); err == nil {
		t.Fatal("bunRequestHead accepted a header value with a line break")
	}
}
