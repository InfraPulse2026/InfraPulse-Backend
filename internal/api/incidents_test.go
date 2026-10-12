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

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

type incidentStub struct{ IncidentRepository }

func (incidentStub) HealthPolicy() health.Policy { return health.Defaults() }
func (incidentStub) Incidents(context.Context, health.Filter) ([]health.Incident, error) {
	return []health.Incident{}, nil
}

type healthCardStub struct {
	IncidentRepository
	items []health.Observation
}

func (s healthCardStub) DeviceHealth(context.Context, string, string) ([]health.Observation, error) {
	return s.items, nil
}
func TestHistoricalSimulationDoesNotPoisonCurrentHealthCategory(t *testing.T) {
	for _, status := range []string{"ok", "stale"} {
		repo := healthCardStub{items: []health.Observation{{Current: false, Assessment: health.Assessment{Status: "stale"}}, {Current: true, Assessment: health.Assessment{Status: status}}}}
		handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDevelopmentAuthDisabled(), WithIncidents(repo))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/devices/device/health?agent_id=agent", nil))
		var response struct {
			Status string `json:"status"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
			t.Fatal("health card failed")
		}
		want := "ok"
		if status == "stale" {
			want = "unknown"
		}
		if response.Status != want {
			t.Fatal("historical simulation affected current health")
		}
	}
}
func (incidentStub) AcknowledgeIncident(ctx context.Context, id string) (health.Incident, error) {
	p, ok := access.Current(ctx)
	if !ok || !access.Allowed(p.User.Role, access.IncidentWrite) {
		return health.Incident{}, access.ErrForbidden
	}
	return health.Incident{ID: id, State: "acknowledged"}, nil
}
func TestIncidentAcknowledgeRequiresOperatorAndConfirmation(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		_, token := authHandler(t, role)
		hash, _ := access.TokenHash(token)
		auth := access.New(accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
		handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithIncidents(incidentStub{}))
		req := httptest.NewRequest(http.MethodPost, "/api/incidents/inc-1/acknowledge", strings.NewReader(`{"confirmed":true}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := 200
		if role == "viewer" {
			want = 403
		}
		if rec.Code != want {
			t.Fatalf("role=%s status=%d body=%s", role, rec.Code, rec.Body.String())
		}
		if role != "viewer" {
			req = httptest.NewRequest(http.MethodPost, "/api/incidents/inc-1/acknowledge", strings.NewReader(`{"confirmed":false}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatal("missing confirmation accepted")
			}
		}
	}
}
