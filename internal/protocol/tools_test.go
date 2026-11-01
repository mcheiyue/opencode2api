package protocol

import (
	"strings"
	"testing"
)

func TestClientToolWireNameIsOpaque(t *testing.T) {
	for _, client := range []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash", "mcp__server__do_thing"} {
		wireName := ClientToolWireName(client, map[string]bool{})
		if !strings.HasPrefix(wireName, ClientToolPrefix) {
			t.Errorf("wire name %q lacks prefix %q", wireName, ClientToolPrefix)
		}
		if len(wireName) > toolWireNameLimit {
			t.Errorf("wire name %q exceeds %d bytes", wireName, toolWireNameLimit)
		}
		if strings.Contains(strings.ToLower(wireName), strings.ToLower(client)) {
			t.Errorf("wire name %q exposes client name %q", wireName, client)
		}
	}
}

func TestClientToolWireNameIsStable(t *testing.T) {
	first := ClientToolWireName("ReportFindings", map[string]bool{})
	second := ClientToolWireName("ReportFindings", map[string]bool{})
	if first != second {
		t.Fatalf("same client tool got %q then %q", first, second)
	}
	if first == ClientToolWireName("ScheduleWakeup", map[string]bool{}) {
		t.Fatalf("different client tools share wire name %q", first)
	}
}

func TestClientToolWireNameSuffixesCollisions(t *testing.T) {
	used := map[string]bool{}
	first := ClientToolWireName("Read", used)
	retry := ClientToolWireName("Read", used)
	if retry == first || !strings.HasSuffix(retry, "_2") {
		t.Fatalf("collision name = %q, want a distinct _2 suffix after %q", retry, first)
	}
}

func TestToolRenamesRestoreOpaqueNames(t *testing.T) {
	renamed := NewToolRenames()
	used := map[string]bool{}
	for _, client := range []string{"ReportFindings", "schedule_wakeup"} {
		wireName := ClientToolWireName(client, used)
		renamed.Add(client, wireName)
		if got := renamed.Restore(wireName); got != client {
			t.Errorf("Restore(%q) = %q, want %q", wireName, got, client)
		}
	}
}
