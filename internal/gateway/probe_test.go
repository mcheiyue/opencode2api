package gateway

import (
	"testing"

	"opencode2api/internal/identity"
)

func TestProbeRequestIDsAreFresh(t *testing.T) {
	first := probeRequestIDs()
	second := probeRequestIDs()
	if first.Session == second.Session {
		t.Errorf("two probes reused session %q", first.Session)
	}
	if first.Project == second.Project {
		t.Errorf("two probes reused project %q", first.Project)
	}
	for _, ids := range []identity.RequestIDs{first, second} {
		if ids.Session == "" || ids.Project == "" {
			t.Fatalf("incomplete identity: %+v", ids)
		}
		if got := identity.CanonicalSessionID(ids.Session); got != ids.Session {
			t.Errorf("probe session %q is not canonical; got %q", ids.Session, got)
		}
		if len(ids.Project) != 40 {
			t.Errorf("probe project %q is not a 40-character id", ids.Project)
		}
	}
}
