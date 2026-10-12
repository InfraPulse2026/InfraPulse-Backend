package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
)

type reportStub struct {
	ReportRepository
	created int
}

func (*reportStub) ReportSummary(_ context.Context, f reports.Filter) (reports.Summary, error) {
	return reports.Summary{Filter: f, Version: "1", Methodology: reports.Methodology()}, nil
}
func (s *reportStub) CreateReportSnapshot(ctx context.Context, f reports.Filter, key string) (reports.Snapshot, bool, error) {
	actor, ok := access.Current(ctx)
	if !ok || !access.Allowed(actor.User.Role, access.ReportWrite) {
		return reports.Snapshot{}, false, access.ErrForbidden
	}
	s.created++
	return reports.Snapshot{ID: "rpt-fixture", Filter: f}, false, nil
}
func TestReportReadAndSnapshotRolesCSRFAndJSONDownload(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		token, err := access.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		hash, _ := access.TokenHash(token)
		auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
		repo := &reportStub{}
		handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithReports(repo))
		req := httptest.NewRequest(http.MethodGet, "/api/reports/summary?download=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatal("public report read")
		}
		req = httptest.NewRequest(http.MethodGet, "/api/reports/summary?download=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("Content-Disposition") == "" {
			t.Fatal("authorized JSON download failed")
		}
		from, to := time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)
		payload, _ := json.Marshal(reports.Filter{From: from, To: to})
		for _, csrf := range []bool{false, true} {
			req = httptest.NewRequest(http.MethodPost, "/api/reports/snapshots", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "report-key")
			req.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
			if csrf {
				req.Header.Set("X-CSRF-Token", access.CSRF(token))
			}
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			want := 403
			if csrf && role != "viewer" {
				want = 201
			}
			if rec.Code != want {
				t.Fatalf("role=%s csrf=%v status=%d", role, csrf, rec.Code)
			}
		}
	}
}
func TestReportRangeQueries(t *testing.T) {
	now := time.Now().UTC()
	for _, path := range []string{"/api/reports/summary?from=bad&to=bad", "/api/reports/summary?from=2026-01-01T00:00:00Z", "/api/reports/summary?top_limit=101", "/api/reports/summary?resource_kind=bogus"} {
		req := httptest.NewRequest("GET", path, nil)
		if _, err := reportFilter(req, now); err == nil {
			t.Fatal("invalid report filter accepted")
		}
	}
	if f, err := reportFilter(httptest.NewRequest("GET", "/api/reports/summary", nil), now); err != nil || f.To.Sub(f.From) != 24*time.Hour {
		t.Fatal("default report period wrong")
	}
}
