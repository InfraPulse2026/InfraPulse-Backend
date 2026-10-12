package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
)

type fakeRepository struct {
	failure   error
	noMetrics bool
}

func (f fakeRepository) TelephonyTargets(context.Context, telephony.Filter) ([]telephony.Target, error) {
	return nil, f.failure
}
func (f fakeRepository) TelephonyTarget(context.Context, string, string) (telephony.Target, error) {
	return telephony.Target{AgentID: "agent-1", TargetID: "pbx", Monitoring: telephony.DescribeInterval("pbx", 30, "backend_default")}, f.failure
}
func (f fakeRepository) TelephonySummary(context.Context, string) (telephony.Summary, error) {
	return telephony.Summary{}, f.failure
}
func (f fakeRepository) TelephonyHistory(context.Context, telephony.HistoryFilter) (telephony.HistoryPage, error) {
	return telephony.HistoryPage{}, f.failure
}
func (f fakeRepository) UpdateTelephonyMonitoring(context.Context, string, string, int) error {
	return f.failure
}

func (f fakeRepository) Logs(context.Context, logbook.Filter) (logbook.Page, error) {
	return logbook.Page{Items: []logbook.Record{}}, f.failure
}
func (f fakeRepository) Log(context.Context, int64, string) (logbook.Record, error) {
	return logbook.Record{}, f.failure
}
func (f fakeRepository) LogsSummary(context.Context, logbook.Filter) (logbook.Summary, error) {
	return logbook.Summary{Severity: map[string]int64{}}, f.failure
}
func (f fakeRepository) AuditEvents(context.Context, logbook.Filter) (storage.AuditPage, error) {
	return storage.AuditPage{Items: []logbook.AuditEvent{}}, f.failure
}

func (f fakeRepository) Ping(context.Context) error                      { return f.failure }
func (f fakeRepository) Agents(context.Context) ([]storage.Agent, error) { return nil, f.failure }
func (f fakeRepository) Devices(context.Context, string, string, int, int) ([]storage.Device, error) {
	return nil, f.failure
}
func (f fakeRepository) Device(context.Context, string, string) (storage.Device, error) {
	return storage.Device{AgentID: "agent-1", DeviceID: "device-1", Inventory: json.RawMessage(`{"id":"device-1"}`), Monitoring: monitoring.Describe(monitoring.Defaults(), "backend_default"), Availability: availability.State{Status: "unknown", Reason: "no_observation"}}, f.failure
}
func (f fakeRepository) UpdateMonitoring(context.Context, string, string, monitoring.Policy) error {
	return f.failure
}
func (f fakeRepository) Topology(context.Context, string, string, bool, int, int) (topology.Graph, error) {
	return topology.Graph{Nodes: []topology.Node{}, Edges: []topology.Edge{}, Collections: []topology.Collection{}}, f.failure
}
func (f fakeRepository) AvailabilitySummary(context.Context, string) (availability.Summary, error) {
	return availability.Summary{}, f.failure
}
func (f fakeRepository) AvailabilityChanges(context.Context, string, string, int, int) ([]availability.Change, error) {
	return nil, f.failure
}
func (f fakeRepository) Metrics(context.Context, string, string, int) ([]storage.Telemetry, error) {
	if f.noMetrics {
		return nil, f.failure
	}
	return []storage.Telemetry{{DeviceID: "device-1", Sample: json.RawMessage(`{"source":"simulated"}`)}}, f.failure
}
func TestAPIResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		repo       fakeRepository
		code       int
		contains   string
	}{
		{"empty", "/api/devices", fakeRepository{}, 200, "items"},
		{"agents", "/api/agents", fakeRepository{}, 200, "items"},
		{"device", "/api/devices/device-1", fakeRepository{}, 200, "device_id"},
		{"inventory", "/api/devices/device-1/inventory", fakeRepository{}, 200, "id"},
		{"monitoring", "/api/devices/device-1/monitoring", fakeRepository{}, 200, "telemetry_interval_seconds"},
		{"metrics", "/api/devices/device-1/metrics", fakeRepository{}, 200, "sample"},
		{"telemetry", "/api/devices/device-1/telemetry", fakeRepository{}, 200, "sample"},
		{"history", "/api/devices/device-1/metrics/history", fakeRepository{}, 200, "items"},
		{"missing", "/api/devices/missing", fakeRepository{failure: storage.ErrNotFound}, 404, "error"},
		{"ambiguous", "/api/devices/router", fakeRepository{failure: storage.ErrAmbiguous}, 409, "error"},
		{"bad-limit", "/api/devices?limit=-1", fakeRepository{}, 400, "error"},
		{"large-limit", "/api/devices/device-1/metrics/history?limit=9999", fakeRepository{}, 400, "error"},
		{"bad-offset", "/api/devices?offset=x", fakeRepository{}, 400, "error"},
		{"storage", "/api/devices", fakeRepository{failure: errors.New("private database address")}, 503, "error"},
		{"readiness", "/readyz", fakeRepository{failure: errors.New("offline")}, 503, "error"},
		{"missing-metrics", "/api/devices/device-1/metrics", fakeRepository{noMetrics: true}, 404, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := testAPI(tc.repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.code {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body[tc.contains]; !ok {
				t.Fatalf("missing %s: %s", tc.contains, w.Body.String())
			}
			if tc.name == "empty" && string(body["items"]) != "[]" {
				t.Fatal("empty list must be an array")
			}
		})
	}
}

func TestMonitoringPolicyUpdates(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"minimum", `{"telemetry_interval_seconds":3,"inventory_interval_seconds":3,"topology_interval_seconds":3}`, 200},
		{"maximum", `{"telemetry_interval_seconds":86400,"inventory_interval_seconds":86400,"topology_interval_seconds":86400}`, 200},
		{"short", `{"telemetry_interval_seconds":2,"inventory_interval_seconds":300,"topology_interval_seconds":300}`, 400},
		{"long", `{"telemetry_interval_seconds":86401,"inventory_interval_seconds":300,"topology_interval_seconds":300}`, 400},
		{"missing", `{"telemetry_interval_seconds":3}`, 400},
		{"fraction", `{"telemetry_interval_seconds":3.5,"inventory_interval_seconds":300,"topology_interval_seconds":300}`, 400},
		{"unknown", `{"telemetry_interval_seconds":3,"inventory_interval_seconds":300,"topology_interval_seconds":300,"apply_now":true}`, 400},
		{"two-objects", `{} {}`, 400},
		{"too-large", strings.Repeat(" ", 2049) + `{}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("PUT", "/api/devices/device-1/monitoring?agent_id=agent-1", strings.NewReader(tc.body)))
			if w.Code != tc.code {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			if tc.code == 200 {
				var p monitoring.Settings
				if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
					t.Fatal(err)
				}
				if p.ApplicationStatus != "unverified" || p.DeliveryMode != "manual_agent_yaml" {
					t.Fatalf("claimed automatic application: %+v", p)
				}
			}
		})
	}
}

func TestOpenAPIIsAvailable(t *testing.T) {
	h := testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/openapi.json", nil))
	var spec struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if w.Code != 200 {
		t.Fatalf("openapi status=%d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	var checkReferences func(any)
	checkReferences = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
				if _, exists := schemas[strings.TrimPrefix(ref, "#/components/schemas/")]; !exists {
					t.Errorf("broken OpenAPI reference %s", ref)
				}
			}
			for _, child := range v {
				checkReferences(child)
			}
		case []any:
			for _, child := range v {
				checkReferences(child)
			}
		}
	}
	checkReferences(document)
	if spec.OpenAPI != "3.0.3" || spec.Info.Version != "0.33.0" || spec.Paths["/api/devices/{id}/telemetry"] == nil || spec.Paths["/api/topology"] == nil || spec.Paths["/api/devices/{id}/topology"] == nil || spec.Paths["/api/availability/summary"] == nil || spec.Paths["/api/devices/{id}/availability"] == nil || spec.Paths["/api/devices/{id}/availability/history"] == nil {
		t.Fatal("incomplete OpenAPI contract")
	}
	for _, path := range []string{"/api/incidents", "/api/incidents/{id}", "/api/incidents/{id}/acknowledge", "/api/incidents/{id}/evidence", "/api/incidents/{id}/transitions", "/api/devices/{id}/health", "/api/health/rules"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
	for _, path := range []string{"/api/reports/summary", "/api/reports/snapshots", "/api/reports/snapshots/{id}"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
	for _, path := range []string{"/api/devices/{id}/interfaces/health", "/api/devices/{id}/interfaces/{interface_id}/health", "/api/telephony/targets/{id}/health"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
	for _, path := range []string{"/api/pki/bootstrap-tokens", "/api/pki/bootstrap-tokens/{id}/revoke", "/api/pki/certificates", "/api/pki/certificates/{id}/revoke", "/api/pki/audit"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
	for _, path := range []string{"/api/logs", "/api/logs/summary", "/api/logs/{id}", "/api/audit", "/api/audit/events", "/api/audit/events/{key}", "/api/devices/{id}/logs", "/api/devices/{id}/audit"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
	for _, path := range []string{"/api/telephony/targets", "/api/telephony/targets/{id}", "/api/telephony/targets/{id}/history", "/api/telephony/targets/{id}/monitoring", "/api/telephony/summary"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI omits %s", path)
		}
	}
}
