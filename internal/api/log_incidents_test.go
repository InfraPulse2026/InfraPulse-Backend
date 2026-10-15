package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type logIncidentStub struct{ incidentStub }

func (logIncidentStub) ReviewLogIncident(ctx context.Context, id string, input health.LogReview) (health.Incident, error) {
	return health.Incident{ID: id, State: "resolved"}, nil
}
func (logIncidentStub) LogIncidentRecords(context.Context, string, int, int) ([]health.LogIncidentRecord, error) {
	severity := uint32(3)
	return []health.LogIncidentRecord{{LogID: 1, BindingID: "binding", Severity: &severity}}, nil
}

func TestLogReviewPermissionsClosedBodyAndEvidence(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		_, token := authHandler(t, role)
		hash, _ := access.TokenHash(token)
		auth := access.New(accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
		h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithIncidents(logIncidentStub{}))
		for _, tc := range []struct {
			body string
			want int
		}{{`{"confirmed":true,"expected_event_count":2,"reason":"Reviewed event"}`, 200}, {`{"confirmed":false,"expected_event_count":2,"reason":"Reviewed event"}`, 400}, {`{"confirmed":true,"expected_event_count":0,"reason":"Reviewed event"}`, 400}, {`{"confirmed":true,"expected_event_count":2,"reason":"Reviewed event","resolve_hardware":true}`, 400}} {
			req := httptest.NewRequest("POST", "/api/incidents/inc-1/review-log-event", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			r := httptest.NewRecorder()
			h.ServeHTTP(r, req)
			want := tc.want
			if role == "viewer" {
				want = 403
			}
			if r.Code != want {
				t.Fatal(role, r.Code, r.Body.String())
			}
		}
		req := httptest.NewRequest("GET", "/api/incidents/inc-1/log-records?raw=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		if r.Code != 200 || strings.Contains(r.Body.String(), `"raw"`) {
			t.Fatal("evidence permission/raw guard", role, r.Code, r.Body.String())
		}
	}
}
