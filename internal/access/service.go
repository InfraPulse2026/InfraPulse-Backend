package access

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"
)

type attempts struct {
	start time.Time
	count int
}
type Service struct {
	Repo     Repository
	Config   Config
	mu       sync.Mutex
	attempts map[[32]byte]attempts
	global   attempts
}

func New(repo Repository, cfg Config) *Service {
	return &Service{Repo: repo, Config: cfg, attempts: make(map[[32]byte]attempts)}
}
func (s *Service) allowLogin(peer, username string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bounded in-memory counters; no permanent account lockout.
	if now.Sub(s.global.start) >= time.Minute {
		s.global = attempts{start: now}
	}
	if s.global.count >= 30 {
		return false
	}
	type pending struct {
		key     [32]byte
		counter attempts
	}
	updates := make([]pending, 0, 2)
	for _, key := range []string{"peer:" + peer, "name:" + username} {
		hash := sha256.Sum256([]byte(key))
		a, exists := s.attempts[hash]
		if !exists && len(s.attempts) >= 2048 {
			for k, v := range s.attempts {
				if now.Sub(v.start) >= 5*time.Minute {
					delete(s.attempts, k)
				}
			}
			if len(s.attempts) >= 2048 {
				return false
			}
		}
		if now.Sub(a.start) >= 5*time.Minute {
			a = attempts{start: now}
		}
		if a.count >= 10 {
			return false
		}
		updates = append(updates, pending{hash, a})
	}
	// Rejected attempts must not consume either another bucket or the shared budget.
	newKeys := func() int {
		n := 0
		for _, u := range updates {
			if _, ok := s.attempts[u.key]; !ok {
				n++
			}
		}
		return n
	}
	if len(s.attempts)+newKeys() > 2048 {
		for k, v := range s.attempts {
			if now.Sub(v.start) >= 5*time.Minute {
				delete(s.attempts, k)
			}
		}
		if len(s.attempts)+newKeys() > 2048 {
			return false
		}
	}
	s.global.count++
	for _, u := range updates {
		u.counter.count++
		s.attempts[u.key] = u.counter
	}
	return true
}

func (s *Service) VerifyCurrentPassword(ctx context.Context, p Principal, password string, meta Meta) (LoginRecord, error) {
	if !s.allowLogin("password:"+meta.Peer, "password:"+p.User.ID, time.Now()) {
		return LoginRecord{}, ErrBusy
	}
	record, err := s.Repo.AccessLoginRecord(ctx, p.User.Username)
	if err != nil {
		return record, err
	}
	if record.Disabled || record.ID != p.User.ID {
		return record, ErrCredentials
	}
	matches, err := CheckPassword(password, record.PasswordHash)
	if err != nil {
		return record, err
	}
	if !matches {
		return record, ErrCredentials
	}
	return record, nil
}
func (s *Service) Login(ctx context.Context, name, password string, meta Meta) (Principal, error) {
	name, err := Username(name)
	if err != nil {
		name = "invalid"
	}
	validName := err == nil
	if !s.allowLogin(meta.Peer, name, time.Now()) {
		return Principal{}, ErrBusy
	}
	if len(password) > 256 {
		return Principal{}, ErrCredentials
	}
	select {
	case passwordGate <- struct{}{}:
		defer func() { <-passwordGate }()
	default:
		return Principal{}, ErrBusy
	}
	record, lookupErr := s.Repo.AccessLoginRecord(ctx, name)
	if !validName {
		lookupErr = ErrCredentials
	}
	if lookupErr != nil && lookupErr != ErrCredentials {
		return Principal{}, lookupErr
	}
	hash := record.PasswordHash
	if lookupErr != nil {
		hash = dummyPasswordHash()
	}
	matches := VerifyPassword(password, hash)
	if !matches || lookupErr != nil || record.Disabled {
		if err := s.Repo.AccessLoginFailure(ctx, meta); err != nil {
			return Principal{}, err
		}
		return Principal{}, ErrCredentials
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	token, err := NewToken()
	if err != nil {
		return Principal{}, err
	}
	tokenHash, _ := TokenHash(token)
	p, err := s.Repo.AccessStartSession(ctx, record, tokenHash, time.Now().UTC().Add(s.Config.SessionTTL), meta)
	if err != nil {
		return Principal{}, err
	}
	p.Token = token
	return p, nil
}
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	var token string
	cookieCount := 0
	for _, c := range r.Cookies() {
		if c.Name == CookieName {
			cookieCount++
			token = c.Value
		}
	}
	auth := r.Header.Values("Authorization")
	fromCookie := cookieCount == 1
	if cookieCount > 1 || len(auth) > 1 || len(auth) > 0 && cookieCount > 0 {
		return Principal{}, ErrCredentials
	}
	if len(auth) == 1 {
		parts := strings.Fields(auth[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return Principal{}, ErrCredentials
		}
		token = parts[1]
		fromCookie = false
	}
	hash, err := TokenHash(token)
	if err != nil {
		return Principal{}, ErrCredentials
	}
	p, err := s.Repo.AccessSession(ctx, hash)
	if err != nil {
		return Principal{}, err
	}
	p.Token, p.Cookie = token, fromCookie
	return p, nil
}
