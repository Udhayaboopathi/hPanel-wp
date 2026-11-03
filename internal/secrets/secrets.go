// Package secrets encrypts sensitive panel fields (per-site DB passwords)
// at rest with AES-256-GCM. The key lives in a 0600 file inside the panel
// data directory and is never logged or served.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

type Box struct{ key []byte }

func LoadOrCreateKey(dir string) (*Box, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "panel.key")
	b, err := os.ReadFile(p)
	if err == nil {
		key, derr := base64.RawStdEncoding.DecodeString(string(b))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("corrupt panel encryption key file")
		}
		return &Box{key: key}, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	enc := base64.RawStdEncoding.EncodeToString(key)
	// Write via a temp file + rename: a crash mid-write with a
	// bare WriteFile leaves a truncated key file, and the next
	// boot then hard-fails with "corrupt panel encryption key
	// file", orphaning every encrypted site DB password.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(enc), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return &Box{key: key}, nil
}

func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	gcm, err := newGCM(b.key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return "enc:v1:" + base64.RawStdEncoding.EncodeToString(ct), nil
}

func (b *Box) Decrypt(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	const prefix = "enc:v1:"
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		// Legacy/plain value (pre-encryption rows) - return as-is.
		return s, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(b.key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(pt), err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
