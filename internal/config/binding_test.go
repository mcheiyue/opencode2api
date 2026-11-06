package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func bindingConfig(t *testing.T) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_keys":["local-test-key"],"zen_keys":["zen-alpha","zen-beta"],"go_keys":["go-gamma"],"proxies":["http://127.0.0.1:18090","http://127.0.0.1:18091","direct"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNormalizeProxyBindings(t *testing.T) {
	cfg := bindingConfig(t)
	cfg.ProxyBindings = map[string]string{
		Fingerprint("zen-alpha"): "http://127.0.0.1:18090",
		"zen-beta":               "http://127.0.0.1:18091",
		"go-gamma":               "direct",
	}
	normalized, err := Normalize("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		Fingerprint("zen-alpha"): "http://127.0.0.1:18090",
		Fingerprint("zen-beta"):  "http://127.0.0.1:18091",
		Fingerprint("go-gamma"):  "direct",
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Bindings map[string]string `json:"proxy_bindings"`
	}
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Bindings) != len(want) {
		t.Fatalf("stored bindings = %v", stored.Bindings)
	}
	for fingerprint, proxy := range want {
		if stored.Bindings[fingerprint] != proxy {
			t.Errorf("binding %q = %q, want %q", fingerprint, stored.Bindings[fingerprint], proxy)
		}
	}
	cfg.ZenKeys[0], cfg.ZenKeys[1] = cfg.ZenKeys[1], cfg.ZenKeys[0]
	cfg.ProxyBindings = normalized.ProxyBindings
	reordered, err := Normalize("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for fingerprint, proxy := range want {
		if reordered.ProxyBindings[fingerprint] != proxy {
			t.Errorf("reordering changed binding %q", fingerprint)
		}
	}
}

func TestNormalizeProxyBindingsRejectsUnknownReferences(t *testing.T) {
	for name, bindings := range map[string]map[string]string{
		"key":   {"unknown-key": "direct"},
		"proxy": {"zen-alpha": "http://192.0.2.1:9999"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := bindingConfig(t)
			cfg.ProxyBindings = bindings
			if _, err := Normalize("", cfg); err == nil {
				t.Fatal("unknown binding reference was accepted")
			}
		})
	}
}

func TestClonePreservesIsolatedProxyBindings(t *testing.T) {
	cfg := bindingConfig(t)
	cfg.ProxyBindings = map[string]string{Fingerprint("zen-alpha"): "direct"}
	cloned := Clone(cfg)
	cloned.ProxyBindings[Fingerprint("zen-alpha")] = cfg.Proxies[0]
	if cfg.ProxyBindings[Fingerprint("zen-alpha")] != "direct" {
		t.Fatal("modifying clone changed original binding")
	}
	if cloned.ProxyBindings[Fingerprint("zen-alpha")] != cfg.Proxies[0] {
		t.Fatal("clone lost binding")
	}
}
