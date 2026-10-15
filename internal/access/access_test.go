package access

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashSaltAndBoundedParameters(t *testing.T) {
	password := "fixture password only"
	a, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || strings.Contains(a, password) || !VerifyPassword(password, a) || VerifyPassword("different fixture", a) {
		t.Fatal("password hashing or salt invalid")
	}
	if VerifyPassword(password, strings.Replace(a, "m=32768", "m=4294967295", 1)) {
		t.Fatal("unbounded hash parameters accepted")
	}
	for _, bad := range []string{"short", strings.Repeat("x", 257)} {
		if _, err = HashPassword(bad); err == nil {
			t.Fatal("bad password accepted")
		}
	}
	if _, err = HashPassword("двенадцать букв и больше"); err != nil {
		t.Fatal("Unicode password rejected")
	}
}

func TestBlockedPeerCannotSpendOtherUsersBudget(t *testing.T) {
	s := New(nil, Config{})
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !s.allowLogin("blocked-peer", "blocked-name", now) {
			t.Fatal("premature limit")
		}
	}
	for i := 0; i < 100; i++ {
		if s.allowLogin("blocked-peer", fmt.Sprintf("other-%d", i), now) {
			t.Fatal("blocked peer accepted")
		}
	}
	if s.global.count != 10 || !s.allowLogin("fresh-peer", "fresh-name", now) {
		t.Fatal("rejected traffic locked out other users")
	}
}

type currentPasswordRepo struct {
	Repository
	calls  int
	record LoginRecord
}

func (r *currentPasswordRepo) AccessLoginRecord(context.Context, string) (LoginRecord, error) {
	r.calls++
	return r.record, nil
}
func TestCurrentPasswordAttemptsAreBoundedBeforeHashWork(t *testing.T) {
	hash, err := HashPassword("fixture password for checks")
	if err != nil {
		t.Fatal(err)
	}
	r := &currentPasswordRepo{record: LoginRecord{User: User{ID: "usr_fixture", Username: "viewer"}, PasswordHash: hash}}
	s := New(r, Config{})
	p := Principal{User: r.record.User}
	ctx := t.Context()
	for i := 0; i < 10; i++ {
		if _, err = s.VerifyCurrentPassword(ctx, p, "wrong", Meta{Peer: "viewer-peer"}); err != ErrCredentials {
			t.Fatal(err)
		}
	}
	before := r.calls
	for i := 0; i < 50; i++ {
		if _, err = s.VerifyCurrentPassword(ctx, p, "wrong", Meta{Peer: "viewer-peer"}); err != ErrBusy {
			t.Fatal("unbounded current-password verification")
		}
	}
	if r.calls != before || !s.allowLogin("another-peer", "another-user", time.Now()) {
		t.Fatal("rejected changes consumed other user capacity")
	}
}
func TestOpaqueSessionsCSRFAndRoles(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := TokenHash(token)
	if err != nil || len(hash) != 32 || strings.Contains(string(hash), token) {
		t.Fatal("token hashing invalid")
	}
	if _, err = TokenHash(token + "="); err == nil {
		t.Fatal("noncanonical token accepted")
	}
	if !EqualCSRF(CSRF(token), CSRF(token)) || EqualCSRF(CSRF(token), CSRF(token+"other")) {
		t.Fatal("CSRF binding invalid")
	}
	if Allowed("viewer", MonitoringWrite) || Allowed("operator", AccessManage) || !Allowed("admin", AccessManage) || Allowed("unknown", DataRead) {
		t.Fatal("roles permit escalation")
	}
}
func TestLoginLimiterMemoryAndWindow(t *testing.T) {
	s := New(nil, Config{})
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !s.allowLogin("peer", "username", now) {
			t.Fatal("premature throttle")
		}
	}
	if s.allowLogin("peer", "username", now) {
		t.Fatal("brute force not throttled")
	}
	if !s.allowLogin("peer", "username", now.Add(6*time.Minute)) {
		t.Fatal("window became a permanent account lock")
	}
	if len(s.attempts) > 2048 {
		t.Fatal("unbounded limiter memory")
	}
}
