package jobs

import "testing"

func TestStateTransitions(t *testing.T) {
	ok := [][2]string{{"queued", "running"}, {"running", "succeeded"}, {"running", "failed"}, {"running", "interrupted"}, {"interrupted", "queued"}, {"failed", "queued"}}
	for _, p := range ok {
		if err := ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("expected %s->%s allowed: %v", p[0], p[1], err)
		}
	}
	bad := [][2]string{{"succeeded", "running"}, {"cancelled", "running"}, {"queued", "succeeded"}, {"bogus", "running"}}
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
