package security

import (
	"os"
	"path/filepath"
	"testing"
)

// SafeJoin must reject paths that escape the root through a
// symlink inside the tree (the lexical prefix check alone passed
// them, which mattered because SafeJoin is shared by the panel
// file manager and the agent).
func TestSafeJoinSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeJoin(root, "link/secret.txt"); err == nil {
		t.Error("expected symlink escape to be rejected")
	}
	// A symlink that stays INSIDE the root is fine.
	inner := filepath.Join(root, "real")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeJoin(root, "alias/new-file.txt"); err != nil {
		t.Errorf("in-root symlink path should be allowed: %v", err)
	}
	// Creating a brand-new file in a plain directory still works.
	if _, err := SafeJoin(root, "real/brand-new.txt"); err != nil {
		t.Errorf("new file in a real directory should be allowed: %v", err)
	}
}
