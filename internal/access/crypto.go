package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const hashPrefix = "$argon2id$v=19$m=32768,t=2,p=1$"

var passwordGate = make(chan struct{}, 2)
var dummyOnce sync.Once
var dummyHash string

func ValidatePassword(p string) error {
	if !utf8.ValidString(p) || utf8.RuneCountInString(p) < 12 || len(p) > 256 {
		return errors.New("password must have at least 12 characters and at most 256 UTF-8 bytes")
	}
	return nil
}
func HashPassword(p string) (string, error) {
	if err := ValidatePassword(p); err != nil {
		return "", err
	}
	select {
	case passwordGate <- struct{}{}:
		defer func() { <-passwordGate }()
	default:
		return "", ErrBusy
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(p), salt, 2, 32768, 1, 32)
	return hashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}
func VerifyPassword(p, encoded string) bool {
	if len(p) > 256 || !strings.HasPrefix(encoded, hashPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, hashPrefix), "$")
	if len(parts) != 2 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil || len(expected) != 32 {
		return false
	}
	digest := argon2.IDKey([]byte(p), salt, 2, 32768, 1, 32)
	return subtle.ConstantTimeCompare(expected, digest) == 1
}
func CheckPassword(p, encoded string) (bool, error) {
	select {
	case passwordGate <- struct{}{}:
		defer func() { <-passwordGate }()
	default:
		return false, ErrBusy
	}
	return VerifyPassword(p, encoded), nil
}
func dummyPasswordHash() string {
	dummyOnce.Do(func() {
		salt := sha256.Sum256([]byte("InfraPulse dummy account, not a credential"))
		digest := argon2.IDKey([]byte("dummy-not-a-user-password"), salt[:16], 2, 32768, 1, 32)
		dummyHash = hashPrefix + base64.RawStdEncoding.EncodeToString(salt[:16]) + "$" + base64.RawStdEncoding.EncodeToString(digest)
	})
	return dummyHash
}
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "usr_" + hex.EncodeToString(b), nil
}
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ip_sess_" + base64.RawURLEncoding.EncodeToString(b), nil
}
func TokenHash(token string) ([]byte, error) {
	if !strings.HasPrefix(token, "ip_sess_") {
		return nil, ErrCredentials
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "ip_sess_"))
	if err != nil || len(b) != 32 || "ip_sess_"+base64.RawURLEncoding.EncodeToString(b) != token {
		return nil, ErrCredentials
	}
	hash := sha256.Sum256([]byte(token))
	return hash[:], nil
}
func CSRF(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("InfraPulse cookie CSRF v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func EqualCSRF(a, b string) bool {
	return len(a) == 43 && len(b) == 43 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
