package admin

import (
	"testing"

	"opencode2api/internal/identity"
)

func TestBenchSessionIsFresh(t *testing.T) {
	first := benchSession()
	second := benchSession()
	if first == "" {
		t.Fatal("empty session")
	}
	if first == second {
		t.Errorf("two bench requests reused session %q", first)
	}
	for _, session := range []string{first, second} {
		if got := identity.CanonicalSessionID(session); got != session {
			t.Errorf("bench session %q is not canonical; got %q", session, got)
		}
	}
}
