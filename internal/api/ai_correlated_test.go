package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type correlatedAIStub struct{ aiStub }

func (s *correlatedAIStub) CreateCorrelatedPreview(ctx context.Context, id string, cfg analysisai.Config) (analysisai.Preview, error) {
	return s.CreatePreview(ctx, id, cfg)
}
func (s *correlatedAIStub) CreateCorrelatedJob(ctx context.Context, id string, input analysisai.Create, key string, cfg analysisai.Config) (analysisai.Job, bool, error) {
	return s.CreateJob(ctx, id, input, key, cfg)
}
func (s *correlatedAIStub) CorrelatedAnalysisJobs(context.Context, string, int) ([]analysisai.Job, error) {
	return []analysisai.Job{}, nil
}
func TestCorrelatedAIPreviewRolesAndConfirmedExecutionCSRF(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		token, _ := access.NewToken()
		hash, _ := access.TokenHash(token)
		auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
		repo := &correlatedAIStub{}
		handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithAI(repo, analysisai.Config{Enabled: true}))
		req := httptest.NewRequest("POST", "/api/correlation/incidents/incident/analysis/preview", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 201 || repo.sent != 0 {
			t.Fatal("preview sent external request or denied read role")
		}
		for _, csrf := range []bool{false, true} {
			req = httptest.NewRequest("POST", "/api/correlation/incidents/incident/analysis", strings.NewReader(`{"preview_id":"preview","request_hash":"`+strings.Repeat("a", 64)+`","confirmed":true}`))
			req.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "key")
			if csrf {
				req.Header.Set("X-CSRF-Token", access.CSRF(token))
			}
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			want := 403
			if csrf && role != "viewer" {
				want = 202
			}
			if rec.Code != want {
				t.Fatalf("role=%s csrf=%v status=%d", role, csrf, rec.Code)
			}
		}
	}
}
