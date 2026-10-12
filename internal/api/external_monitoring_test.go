package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
)

type externalStub struct {
	assetStub
	calls int
}

func (s *externalStub) ExternalMetrics(context.Context, externalmonitor.Filter, bool) ([]externalmonitor.MetricRecord, error) {
	s.calls++
	return []externalmonitor.MetricRecord{}, nil
}
func (s *externalStub) ExternalSignals(context.Context, externalmonitor.Filter) ([]externalmonitor.SignalRecord, error) {
	s.calls++
	return []externalmonitor.SignalRecord{}, nil
}
func (s *externalStub) ExternalSignalHistory(context.Context, string, string, int, int) ([]externalmonitor.SignalVersion, error) {
	s.calls++
	return []externalmonitor.SignalVersion{}, nil
}
func TestExternalMonitoringRoutesRolesAndBounds(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &externalStub{}
			handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})), WithAssets(repo))
			for _, path := range []string{"/api/signals", "/api/assets/a/signals", "/api/assets/a/external-metrics", "/api/data-sources/s/metrics", "/api/data-sources/s/metrics/history", "/api/data-sources/s/signals/11/history"} {
				request := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 401 {
					t.Fatal("unprotected monitoring", path, w.Code)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 200 {
					t.Fatal("read denied", role, path, w.Code)
				}
			}
			before := repo.calls
			for _, path := range []string{"/api/signals?state=healthy", "/api/signals?limit=201", "/api/signals?offset=-1", "/api/data-sources/s/metrics?item_id=01", "/api/data-sources/s/signals/invalid/history"} {
				request := httptest.NewRequest("GET", path, nil)
				request.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 400 || repo.calls != before {
					t.Fatal("invalid request reached storage", path, w.Code)
				}
			}
		})
	}
}
