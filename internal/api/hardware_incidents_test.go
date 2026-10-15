package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHardwareIncidentFilterUsesExistingAPI(t *testing.T) {
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDevelopmentAuthDisabled(), WithIncidents(incidentStub{}))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/api/incidents?resource_kind=hardware", nil))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestOpenAPIHardwareIncidentEvidence(t *testing.T) {
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/openapi.json", nil))
	var d struct {
		Components struct {
			Schemas map[string]struct{ Properties map[string]json.RawMessage }
		}
	}
	if json.Unmarshal(r.Body.Bytes(), &d) != nil {
		t.Fatal("invalid OpenAPI")
	}
	if len(d.Components.Schemas["HealthAssessment"].Properties["hardware"]) == 0 || len(d.Components.Schemas["HardwareIncidentEvidence"].Properties["sample_id"]) == 0 {
		t.Fatal("hardware evidence missing")
	}
	if !strings.Contains(string(d.Components.Schemas["Incident"].Properties["evidence_status"]), `"clock_degraded"`) {
		t.Fatal("hardware freshness missing from API contract")
	}
}
