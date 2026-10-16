package rotation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"opencode2api/internal/config"
)

func TestConfiguredOrderExcludesDiscoveredAndPersistedModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.json")
	initial := Snapshot{Groups: []*Group{
		{ID: "sota", Alias: "sota", Order: config.SOTAModels, Current: config.SOTAModels[0]},
		{ID: "sweet", Alias: "sweet", Order: []string{"configured-free", "persisted-free"}, Current: "persisted-free"},
	}}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.RotationConfig{
		Sweet: config.RotationGroup{Alias: "sweet", Order: []string{"configured-free"}},
	}
	manager, err := New(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.Sync(map[string]bool{
		"configured-free": true,
		"discovered-free": true,
	})

	sweet := manager.Snapshot().Groups[1]
	if !reflect.DeepEqual(sweet.Order, []string{"configured-free"}) {
		t.Fatalf("Sweet order = %v, want only configured models", sweet.Order)
	}
	if sweet.Current != "configured-free" {
		t.Fatalf("Sweet current = %q, want configured-free", sweet.Current)
	}
	if !reflect.DeepEqual(sweet.Available, map[string]bool{"configured-free": true}) {
		t.Fatalf("Sweet available = %v, want only configured models", sweet.Available)
	}
}
