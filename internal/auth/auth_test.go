package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "argon2id$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !VerifyPassword(h, "correct horse battery staple") {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword(h, "wrong password") {
		t.Fatal("wrong password verified")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected short password to be rejected")
	}
}

func TestGenerateSecurePassword(t *testing.T) {
	a, err := GenerateSecurePassword(24)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateSecurePassword(24)
	if len(a) != 24 || a == b {
		t.Fatalf("passwords not random/expected length: %q %q", a, b)
	}
}

func TestTokenHashing(t *testing.T) {
	plain, hash, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if plain == hash || HashToken(plain) != hash {
		t.Fatal("token hash mismatch")
	}
}

func TestRecoveryCodes(t *testing.T) {
	plains, hashes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plains) != 10 || len(hashes) != 10 {
		t.Fatal("expected 10 codes")
	}
	if RecoveryCodeHash(plains[0]) != hashes[0] {
		t.Fatal("recovery hash does not match plain code")
	}
	// Normalization: lowercase + missing dash still matches.
	lower := strings.ToLower(strings.ReplaceAll(plains[0], "-", ""))
	if RecoveryCodeHash(lower) != hashes[0] {
		t.Fatal("normalized recovery code should match")
	}
}

func TestTOTP(t *testing.T) {
	secret, url, err := GenerateTOTPSecret("admin")
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || !strings.Contains(url, "otpauth://") {
		t.Fatalf("bad totp generation: %q %q", secret, url)
	}
	if ValidateTOTP(secret, "000000") {
		// Extremely unlikely but theoretically possible once in 10^6 runs;
		// accept failure signal by checking a clearly invalid format too.
		if ValidateTOTP(secret, "abcdef") {
			t.Fatal("non-numeric code validated")
		}
	}
}
