package wordpress

import (
	"strings"
	"testing"
)

// The panel's password reset previously built
// `wp user reset-password --new-pass=…`, an option WP-CLI does
// not support, so the operation always failed. The reset must go
// through `user update --user_pass`.
func TestResetPasswordArgsUsesUserUpdate(t *testing.T) {
	args := ResetPasswordArgs("admin", "S3cret-pass")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--new-pass") {
		t.Fatalf("args must not contain the unsupported --new-pass option: %v", args)
	}
	if !strings.Contains(joined, "user update admin") {
		t.Fatalf("args should update the user: %v", args)
	}
	if !strings.Contains(joined, "--user_pass=S3cret-pass") {
		t.Fatalf("args should carry the new password via --user_pass: %v", args)
	}
	if !strings.Contains(joined, "--skip-email") {
		t.Fatalf("args should skip the notification email: %v", args)
	}
}
