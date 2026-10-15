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

type resourceAPIStub struct {
	IncidentRepository
	kind, resource string
}

func (s *resourceAPIStub) ResourceHealth(_ context.Context, _, _, kind, resource string) ([]health.Observation, error) {
	s.kind = kind
	s.resource = resource
	return []health.Observation{{Current: true, ResourceKind: kind, ResourceID: resource, Assessment: health.Assessment{Status: "warning"}}}, nil
}
func TestResourceHealthReadsRequireAuthenticationAndRouteKinds(t *testing.T) {
	token, err := access.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := access.TokenHash(token)
	auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: "viewer"}, SessionHash: hash}}, access.Config{Mode: "required"})
	repo := &resourceAPIStub{}
	handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithIncidents(repo))
	for _, tc := range []struct{ path, kind, id string }{{"/api/devices/device/interfaces/port-1/health", "interface", "port-1"}, {"/api/devices/device/interfaces/health", "interface", ""}, {"/api/telephony/targets/sip-1/health", "sip", "sip-1"}, {"/api/agents/agent-1/health", "agent", "agent-1"}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatal("public resource read")
		}
		req = httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var result map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result["status"] != "warning" || result["coverage"] != "observed_components_only" || repo.kind != tc.kind || repo.resource != tc.id {
			t.Fatalf("wrong resource DTO/route: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}
