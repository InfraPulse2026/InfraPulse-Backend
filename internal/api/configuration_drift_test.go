package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type driftAPIStub struct{ calls int }

func (s *driftAPIStub) ConfigurationBaseline(context.Context, string) (cd.Baseline, error) {
	s.calls++
	return cd.Baseline{Revision: 1}, nil
}
func (s *driftAPIStub) PutConfigurationBaseline(context.Context, string, cd.BaselineInput, string) (cd.Baseline, bool, error) {
	s.calls++
	return cd.Baseline{Revision: 1}, true, nil
}
func (s *driftAPIStub) ConfigurationBaselineHistory(context.Context, string, int, int) ([]cd.Baseline, error) {
	s.calls++
	return []cd.Baseline{}, nil
}
func (s *driftAPIStub) ConfigurationDrift(context.Context, string) (cd.State, error) {
	s.calls++
	return cd.State{Status: "unknown"}, nil
}
func (s *driftAPIStub) ConfigurationDriftHistory(context.Context, string, int, int) ([]cd.Record, error) {
	s.calls++
	return []cd.Record{}, nil
}
func (s *driftAPIStub) CompareConfiguration(context.Context, string, cd.CompareInput) (cd.Record, error) {
	s.calls++
	return cd.Record{}, nil
}
func TestConfigurationDriftRouteRolesClosedBodiesAndNoExecution(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &driftAPIStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithConfigurationDrift(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/assets/asset_one/configuration/drift", "/api/assets/asset_one/configuration/drift/history", "/api/assets/asset_one/configuration/baseline", "/api/assets/asset_one/configuration/baseline/history"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("anonymous", w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 200
				if strings.Contains(path, "baseline") && role != "admin" {
					want = 403
				}
				if w.Code != want || w.Header().Get("Cache-Control") != "no-store" && want == 200 {
					t.Fatal(path, w.Code)
				}
			}
			body := `{"expected_revision":0,"asset_revision":1,"snapshot_id":"snapshot_one","confirmed":true,"reason":"reviewed baseline"}`
			r := httptest.NewRequest("PUT", "/api/assets/asset_one/configuration/baseline", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", "baseline-key")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 403
			if role == "admin" {
				want = 200
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			if role == "admin" {
				if w.Header().Get("Idempotent-Replay") != "true" {
					t.Fatal("replay header")
				}
				before := repo.calls
				for _, bad := range []string{`{"snapshot_id":"snapshot_one","configuration":"username secret"}`, strings.Replace(body, `"confirmed":true`, `"confirmed":false`, 1), body + ` {}`, strings.Repeat("x", 2049)} {
					r = httptest.NewRequest("PUT", "/api/assets/asset_one/configuration/baseline", strings.NewReader(bad))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "bad-key")
					w = httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 400 || repo.calls != before {
						t.Fatal("invalid baseline reached storage", w.Code)
					}
				}
			}
			r = httptest.NewRequest("POST", "/api/assets/asset_one/configuration/compare", strings.NewReader(`{"baseline_revision":1,"snapshot_id":"snapshot_one","reason":"review comparison"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want = 200
			if role == "viewer" {
				want = 403
			}
			if w.Code != want {
				t.Fatal("compare", w.Code)
			}
			r = httptest.NewRequest("POST", "/api/assets/asset_one/configuration/restore", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatal("unimplemented restore appears executable", w.Code)
			}
		})
	}
}
