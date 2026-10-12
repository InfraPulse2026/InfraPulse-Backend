package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	cc "github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type configAPIStub struct{ calls int }

func (s *configAPIStub) CollectorConfig(context.Context, string) (cc.State, error) {
	s.calls++
	return cc.State{}, nil
}
func (s *configAPIStub) PutCollectorConfig(context.Context, string, cc.Input, string) (cc.Revision, bool, error) {
	s.calls++
	return cc.Revision{Revision: 1}, true, nil
}
func (s *configAPIStub) CollectorConfigHistory(context.Context, string, int, int) ([]cc.Revision, error) {
	s.calls++
	return []cc.Revision{}, nil
}
func (s *configAPIStub) CollectorConfigEvents(context.Context, string, int, int) ([]cc.Event, error) {
	s.calls++
	return []cc.Event{}, nil
}
func TestCollectorConfigAdministratorRoutesAndStrictInput(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &configAPIStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithCollectorConfig(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/collectors/collector/config", "/api/collectors/collector/config/history", "/api/collectors/collector/config/events"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("anonymous", w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 403
				if role == "admin" {
					want = 200
				}
				if w.Code != want {
					t.Fatal("management read permissions", w.Code)
				}
			}
			body := `{"expected_revision":0,"confirmed":true,"reason":"reviewed target","targets":[]}`
			r := httptest.NewRequest("PUT", "/api/collectors/collector/config", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "managed-config")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 403
			if role == "admin" {
				want = 202
			}
			if w.Code != want {
				t.Fatal("management writes", w.Code, w.Body.String())
			}
			if role != "admin" && repo.calls != 0 {
				t.Fatal("forbidden request reached repository")
			}
			if role == "admin" {
				if w.Header().Get("Idempotent-Replay") != "true" {
					t.Fatal("replay header")
				}
				before := repo.calls
				for _, bad := range []string{`{"targets":[],"remote_management":{"enabled":true}}`, `{"targets":[]} {}`, `{"targets":[]`, strings.Repeat("x", cc.MaxBytes+1)} {
					r := httptest.NewRequest("PUT", "/api/collectors/collector/config", strings.NewReader(bad))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "bad")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 400 || repo.calls != before {
						t.Fatal("invalid input reached repository", w.Code)
					}
				}
			}
		})
	}
}
