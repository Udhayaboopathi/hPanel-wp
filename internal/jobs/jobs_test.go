package jobs

import "testing"

func TestStateTransitions(t *testing.T) {
	ok := [][2]string{{"queued", "running"}, {"running", "succeeded"}, {"running", "failed"}, {"running", "interrupted"}, {"running", "queued"}}
	for _, p := range ok {
		if err := ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("expected %s->%s allowed: %v", p[0], p[1], err)
		}
	}
	// failed->queued and interrupted->queued are deliberately NOT
	// transitions: re-queueing a finished job by state change alone
	// resurrects it with no executor behind it (a real defect the
	// runner's Retry API already refuses). Retries enqueue a FRESH
	// job; the bounded auto-retry writes its state directly.
	bad := [][2]string{{"succeeded", "running"}, {"cancelled", "running"}, {"queued", "succeeded"}, {"bogus", "running"}, {"failed", "queued"}, {"interrupted", "queued"}}
	for _, p := range bad {
		if err := ValidateTransition(p[0], p[1]); err == nil {
			t.Errorf("expected %s->%s rejected", p[0], p[1])
		}
	}
	if !IsTerminal("succeeded") || IsTerminal("running") || !IsActive("queued") {
		t.Error("terminal/active helpers wrong")
	}
	if err := ValidateKind("nuke_everything"); err == nil {
		t.Error("expected non-allowlisted kind rejected")
	}
	if err := ValidateKind("create_site"); err != nil {
		t.Error("create_site should be allowlisted")
	}
}
