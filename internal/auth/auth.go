package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP recommended for interactive login).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

func HashPassword(password string) (string, error) {
	if len(password) < 10 {
		return "", fmt.Errorf("password must be at least 10 characters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(h)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var m, t, p uint32
	if _, err := fmt.Sscanf(parts[1], "v=19"); err != nil {
		// parts[1] is version segment; params are parts[2]
		_ = err
	}
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789!@#$%^&*()-_=+"

func GenerateSecurePassword(length int) (string, error) {
	if length < 16 {
		length = 24
	}
	out := make([]byte, length)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// GenerateToken returns a URL-safe random token and its SHA-256 hash.
// Only the hash is stored server-side.
func GenerateToken() (plain, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// TOTP

func GenerateTOTPSecret(account string) (secret, url string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "Own hPanel",
		AccountName: account,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

func ValidateTOTP(secret, code string) bool {
	return totp.Validate(strings.TrimSpace(code), secret)
}

// Recovery codes: shown once to the user, stored only as SHA-256 hashes.
func GenerateRecoveryCodes(n int) ([]string, []string, error) {
	plains := make([]string, 0, n)
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		code := strings.ToUpper(hex.EncodeToString(b))
		code = code[:5] + "-" + code[5:]
		plains = append(plains, code)
		hashes = append(hashes, HashToken(normalizeRecovery(code)))
	}
	return plains, hashes, nil
}

func normalizeRecovery(c string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), "-", ""))
}

func RecoveryCodeHash(code string) string { return HashToken(normalizeRecovery(code)) }

// SessionExpiry is the absolute server-side session lifetime.
const SessionExpiry = 12 * time.Hour

func NewSessionExpiry() time.Time { return time.Now().UTC().Add(SessionExpiry) }
