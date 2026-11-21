package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/oauth"
	"opencode2api/internal/protocol"
)

func TestLoadDefaultsStreamContinuationTimeoutForExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"performance":{"stream_continuation":true}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy config: %v", err)
	}
	if got := cfg.Performance.StreamContinuationTimeoutSeconds; got != 60 {
		t.Fatalf("stream_continuation_timeout_seconds = %d, want 60", got)
	}
	if got := cfg.Retry.TimeoutSeconds; got != 300 {
		t.Fatalf("retry.timeout_seconds = %d, want 300", got)
	}
	if got := cfg.Performance.StreamContinuationMax; got != 1 {
		t.Fatalf("stream_continuation_max = %d, want 1", got)
	}
}

func TestLoadLegacyOAuthAliasesAndExplicitAnonymous(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"go_subscription":{"credential":{"access_token":"access","refresh_token":"refresh","expires_at":"2030-01-01T00:00:00Z"},"routes":{"model":{"model_id":"upstream-model","protocol":"chat","base_url":"https://opencode.ai/inference/go/openai"}}}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy OAuth config: %v", err)
	}
	if !cfg.Anonymous || cfg.GoOAuth == nil || cfg.GoOAuth.Credential.AccessToken != "access" {
		t.Fatalf("legacy configuration not preserved: anonymous=%v, go_oauth=%+v", cfg.Anonymous, cfg.GoOAuth)
	}
	if _, err := migrateOAuthFields([]byte(`{"server_keys":["local-test"],"go_oauth":{},"go_subscription":{}}`)); err == nil {
		t.Fatal("mixed legacy and current OAuth fields were accepted")
	}
}

func TestSaveAtomicPersistsOAuthCredentialsAndRedactsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Config{
		Listen: "127.0.0.1:8080", ServerKeys: []string{"local-test"}, Anonymous: false,
		Warp:        WarpConfig{PoolSize: 2, Stack: "ipv6"},
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300},
		Performance: PerformanceConfig{MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeoutSeconds: 1, ConnectTimeoutSeconds: 1, FailureCooldownSeconds: 1},
		Logging:     LoggingConfig{Level: "info", RingSize: 100},
		Prefer:      TierGo,
		GoOAuth: &oauth.Connection{
			Credential: oauth.Credential{AccessToken: "secret-access-token", RefreshToken: "secret-refresh-token", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
			Routes:     map[string]oauth.Route{"model": {ModelID: "remote", Protocol: protocol.Chat, BaseURL: "https://opencode.ai/inference/go/openai"}},
		},
	}
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.GoOAuth == nil || saved.GoOAuth.Credential.AccessToken != "secret-access-token" || saved.GoOAuth.Credential.RefreshToken != "secret-refresh-token" {
		t.Fatal("OAuth credentials were not persisted")
	}
	redactor := SecretRedactor()
	redactor.Replace(cfg)
	if got := redactor.String("secret-access-token secret-refresh-token"); strings.Contains(got, "secret-") {
		t.Fatalf("OAuth credentials were not redacted: %q", got)
	}
}

func TestLoadPreservesQuotaAndPromptEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"retry":{"quota_gate_seconds":45,"max_retry_after_seconds":90},"upstream":{"prompt_environment":{"working_directory":" /srv/app ","workspace_root":"/srv","platform":"windows","is_git_repo":true}}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if cfg.Retry.QuotaGateSeconds != 45 || cfg.Retry.MaxRetryAfterSeconds != 90 {
		t.Fatalf("retry settings lost: %+v", cfg.Retry)
	}
	if environment := cfg.Upstream.PromptEnvironment; environment == nil || environment.WorkingDirectory != "/srv/app" || environment.WorkspaceRoot != "/srv" || environment.Platform != "win32" || environment.IsGitRepo == nil || !*environment.IsGitRepo {
		t.Fatalf("prompt environment not normalized: %+v", environment)
	}
}

func TestNormalizeRejectsOAuthRoutesFromOtherPlan(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:8080", ServerKeys: []string{"local-test"}, Prefer: TierGo,
		GoOAuth: &oauth.Connection{
			Credential: oauth.Credential{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
			Routes:     map[string]oauth.Route{"model": {ModelID: "remote", Protocol: protocol.Chat, BaseURL: "https://opencode.ai/inference/openai"}},
		},
	}
	if _, err := Normalize("", cfg); err == nil {
		t.Fatal("Go connection with a Zen route was accepted")
	}
}

func TestLoadStreamContinuationTimeoutOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"performance":{"stream_continuation_timeout_seconds":90}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got := cfg.Performance.StreamContinuationTimeoutSeconds; got != 90 {
		t.Fatalf("stream_continuation_timeout_seconds = %d, want 90", got)
	}
}
