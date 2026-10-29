package auth

import (
	"errors"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost matches Python's bcrypt.gensalt() default.
const bcryptCost = 12

// maxBcryptBytes is bcrypt's input limit.
const maxBcryptBytes = 72

// ErrPasswordTooLong is returned by HashPassword for inputs over 72 bytes.
var ErrPasswordTooLong = errors.New("password cannot be longer than 72 bytes")

// HashPassword returns a $2b$ cost-12 hash. Like the installed Python bcrypt
// (5.x), it refuses passwords over 72 bytes instead of truncating.
func HashPassword(pw string) (string, error) {
	if len(pw) > maxBcryptBytes {
		return "", ErrPasswordTooLong
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword never panics and never errors up: any failure is false, so a
// login answers 401 rather than 500. Passwords over 72 bytes are false:
// Python bcrypt 5.x raises ValueError there (which FastAPI would turn into a
// 500); Go deliberately rejects instead. An empty password is always false.
func VerifyPassword(pw, hash string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	if pw == "" || len(pw) > maxBcryptBytes || hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// DummyVerify burns one cost-12 bcrypt compare so unknown users cost the same
// as known ones.
func DummyVerify() {
	dummyOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcryptCost)
	})
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte("not-the-password"))
}
