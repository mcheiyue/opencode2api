package warp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A non-2xx registration reply can echo the device token, so it must reach the
// caller as a bare status and never as logged text.
func TestRegistrarErrorOmitsResponseBody(t *testing.T) {
	const secret = "cf-device-token-0123456789"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"` + secret + `"}]}`))
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: newRegistrar().client}
	err := r.doJSON(context.Background(), http.MethodPost, "/reg", []byte(`{}`), "", &deviceEnvelope{})
	if err == nil {
		t.Fatal("doJSON accepted a 400 reply")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the response body: %v", err)
	}
	if got, want := err.Error(), "Cloudflare API HTTP 400"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: a registration POST must not be replayed", got)
	}
}

// A 5xx registration reply is not proof the server ignored the POST, so it is
// reported at once instead of being replayed into duplicate devices.
func TestRegistrarDoesNotReplayUnsafeMethods(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: newRegistrar().client}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		requests.Store(0)
		if err := r.doJSON(context.Background(), method, "/reg", []byte(`{}`), "token", &deviceEnvelope{}); err == nil {
			t.Fatalf("%s: doJSON accepted a 500 reply", method)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("%s: requests = %d, want 1 (no replay)", method, got)
		}
	}
}

// Reading the device is safe to replay, so one transient 5xx does not fail the
// registration that already created the device.
func TestRegistrarDoesNotReplayReadsWithBodies(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: newRegistrar().client}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		requests.Store(0)
		if err := r.doJSON(context.Background(), method, "/reg/device", []byte(`{}`), "token", &deviceEnvelope{}); err == nil {
			t.Fatalf("%s: doJSON accepted a 500 reply", method)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("%s with body: requests = %d, want 1 (no replay)", method, got)
		}
	}
}

func TestRegistrarRetriesSafeReads(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":"device","token":"token","warp_enabled":true}}`))
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: newRegistrar().client}
	var device deviceEnvelope
	if err := r.doJSON(context.Background(), http.MethodGet, "/reg/device", nil, "token", &device); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if device.Result.ID != "device" || !device.Result.WARPEnabled {
		t.Errorf("device = %+v, want the decoded result", device.Result)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2 (one retry)", got)
	}
}

// Credentials must never reach the origin a redirect points at.
func TestRegistrarDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	defer other.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/reg", http.StatusFound)
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: newRegistrar().client}
	err := r.doJSON(context.Background(), http.MethodPost, "/reg", []byte(`{}`), "device-token", &deviceEnvelope{})
	if err == nil {
		t.Fatal("doJSON accepted a redirect")
	}
	if got, want := err.Error(), "Cloudflare API HTTP 302"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if got := elsewhere.Load(); got != 0 {
		t.Errorf("redirect target received %d requests, want 0", got)
	}
}
