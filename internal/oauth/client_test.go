package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/protocol"
)

func TestNormalizeBaseForPlanKeepsOAuthRoutesPlanIsolated(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		plan     Plan
		wantURL  string
		wantOkay bool
	}{
		{"Go inference route", "https://opencode.ai/inference/go/openai", PlanGo, "https://opencode.ai/inference/go/openai", true},
		{"Zen inference route", "https://opencode.ai/inference/openai", PlanZen, "https://opencode.ai/inference/openai", true},
		{"Go rejects Zen route", "https://opencode.ai/inference/openai", PlanGo, "", false},
		{"Zen rejects Go route", "https://opencode.ai/inference/go/openai", PlanZen, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, okay := normalizeBaseForPlan(tt.baseURL, protocol.Chat, tt.plan)
			if got != tt.wantURL || okay != tt.wantOkay {
				t.Fatalf("normalizeBaseForPlan() = (%q, %v), want (%q, %v)", got, okay, tt.wantURL, tt.wantOkay)
			}
		})
	}
}

func TestClientStartPollAndRefreshUseOAuthContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ConsoleURL is https://opencode.ai/console, so every request path is
		// built under that prefix.
		switch r.URL.Path {
		case "/console/auth/device/code":
			var request struct {
				ClientID         string `json:"client_id"`
				SupportsOrgScope bool   `json:"supports_org_scope"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ClientID != clientID || !request.SupportsOrgScope {
				http.Error(w, "invalid device request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"device","user_code":"ABCD","verification_uri_complete":"https://opencode.ai/console/device","expires_in":600,"interval":5}`))
		case "/console/auth/device/token":
			var request struct {
				GrantType    string `json:"grant_type"`
				DeviceCode   string `json:"device_code"`
				RefreshToken string `json:"refresh_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			switch request.GrantType {
			case "urn:ietf:params:oauth:grant-type:device_code":
				if request.DeviceCode != "device" {
					http.Error(w, "invalid device code", http.StatusBadRequest)
					return
				}
			case "refresh_token":
				if request.RefreshToken != "refresh-old" {
					http.Error(w, "invalid refresh token", http.StatusBadRequest)
					return
				}
			default:
				http.Error(w, "invalid grant", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600,"org_id":"org-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client())
	client.http.Transport = rewriteConsoleTransport{base: server.URL, next: server.Client().Transport}
	device, err := client.Start(context.Background())
	if err != nil {
		t.Fatalf("Start device flow: %v", err)
	}
	if device.UserCode != "ABCD" || device.VerificationURI != "https://opencode.ai/console/device" || device.Interval != 5*time.Second || device.DeviceCode != "device" {
		t.Fatalf("unexpected device response: %+v", device)
	}
	token, err := client.Poll(context.Background(), device.DeviceCode)
	if err != nil {
		t.Fatalf("Poll device flow: %v", err)
	}
	if token.AccessToken != "access-new" || token.RefreshToken != "refresh-new" || token.OrganizationID != "org-1" {
		t.Fatalf("unexpected device token: %+v", token)
	}
	credential, err := client.Refresh(context.Background(), Credential{
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour), OrganizationID: "org-1",
	})
	if err != nil {
		t.Fatalf("Refresh credential: %v", err)
	}
	if credential.AccessToken != "access-new" || credential.RefreshToken != "refresh-new" || credential.OrganizationID != "org-1" {
		t.Fatalf("unexpected refreshed credential: %+v", credential)
	}
}

type rewriteConsoleTransport struct {
	base string
	next http.RoundTripper
}

func (transport rewriteConsoleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme = "http"
	copy.URL.Host = strings.TrimPrefix(transport.base, "http://")
	copy.Host = copy.URL.Host
	return transport.next.RoundTrip(copy)
}
