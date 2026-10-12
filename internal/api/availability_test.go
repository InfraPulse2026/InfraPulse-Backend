package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

type availabilityRepository struct {
	fakeRepository
	deviceFunc  func(context.Context, string, string) (storage.Device, error)
	summaryFunc func(context.Context, string) (availability.Summary, error)
	changesFunc func(context.Context, string, string, int, int) ([]availability.Change, error)
}

func (f availabilityRepository) Device(ctx context.Context, agentID, deviceID string) (storage.Device, error) {
	if f.deviceFunc != nil {
		return f.deviceFunc(ctx, agentID, deviceID)
	}
	return f.fakeRepository.Device(ctx, agentID, deviceID)
}

func (f availabilityRepository) AvailabilitySummary(ctx context.Context, agentID string) (availability.Summary, error) {
	if f.summaryFunc != nil {
		return f.summaryFunc(ctx, agentID)
	}
	return f.fakeRepository.AvailabilitySummary(ctx, agentID)
}

func (f availabilityRepository) AvailabilityChanges(ctx context.Context, agentID, deviceID string, limit, offset int) ([]availability.Change, error) {
	if f.changesFunc != nil {
		return f.changesFunc(ctx, agentID, deviceID, limit, offset)
	}
	return f.fakeRepository.AvailabilityChanges(ctx, agentID, deviceID, limit, offset)
}

func availabilityRequest(repo Repository, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testAPI(repo, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestDeviceAvailabilityReturnsRepositoryState(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, status := range []string{"online", "offline", "stale", "unknown"} {
		t.Run(status, func(t *testing.T) {
			reported, age := true, int64(5)
			state := availability.State{Status: status, Reason: "device_reported_online", LastReportedOnline: &reported, AgentConnected: true, EvidenceKind: "telemetry", Source: "remote", MessageID: "message-1", TimeBasis: "observed_at", ObservedAt: &now, ReceivedAt: &now, ExpiresAt: &now, AgeSeconds: &age, ExpectedIntervalSeconds: 3, StaleAfterSeconds: 11, ClockSkew: true}
			if status == "unknown" {
				state = availability.State{Status: "unknown", Reason: "no_observation"}
			}
			repo := availabilityRepository{deviceFunc: func(_ context.Context, agentID, deviceID string) (storage.Device, error) {
				if agentID != "agent-query" || deviceID != "router" {
					t.Fatalf("device lookup = (%q, %q)", agentID, deviceID)
				}
				return storage.Device{Availability: state}, nil
			}}
			w := availabilityRequest(repo, "/api/devices/router/availability?agent_id=agent-query")
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var got availability.State
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, state) {
				t.Fatalf("state changed in API: got %+v, want %+v", got, state)
			}
		})
	}
}

func TestAvailabilitySummaryFilterAndCounts(t *testing.T) {
	for _, agentID := range []string{"", "agent-1"} {
		t.Run(agentID, func(t *testing.T) {
			want := availability.Summary{GeneratedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), Total: 4, Online: 1, Offline: 1, Stale: 1, Unknown: 1, ClockSkew: 1, AgentDisconnected: 2}
			repo := availabilityRepository{summaryFunc: func(_ context.Context, gotAgentID string) (availability.Summary, error) {
				if gotAgentID != agentID {
					t.Fatalf("agent filter=%q, want %q", gotAgentID, agentID)
				}
				return want, nil
			}}
			w := availabilityRequest(repo, "/api/availability/summary?agent_id="+agentID)
			var got availability.Summary
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("summary=%+v, want %+v", got, want)
			}
		})
	}
}

func TestAvailabilityHistoryUsesResolvedDeviceAndPagination(t *testing.T) {
	for _, tc := range []struct {
		query         string
		limit, offset int
	}{
		{"", 100, 0},
		{"?agent_id=agent-query&limit=1000&offset=1000000", 1000, 1000000},
	} {
		t.Run(tc.query, func(t *testing.T) {
			deviceCalls, historyCalls := 0, 0
			repo := availabilityRepository{
				deviceFunc: func(_ context.Context, agentID, deviceID string) (storage.Device, error) {
					deviceCalls++
					if deviceID != "router" || (tc.query != "" && agentID != "agent-query") {
						t.Fatalf("device lookup = (%q, %q)", agentID, deviceID)
					}
					return storage.Device{AgentID: "resolved-agent", DeviceID: "resolved-device"}, nil
				},
				changesFunc: func(_ context.Context, agentID, deviceID string, limit, offset int) ([]availability.Change, error) {
					historyCalls++
					if deviceCalls != 1 || agentID != "resolved-agent" || deviceID != "resolved-device" || limit != tc.limit || offset != tc.offset {
						t.Fatalf("history lookup = (%q, %q, %d, %d), deviceCalls=%d", agentID, deviceID, limit, offset, deviceCalls)
					}
					return nil, nil
				},
			}
			w := availabilityRequest(repo, "/api/devices/router/availability/history"+tc.query)
			var got struct {
				Items  json.RawMessage `json:"items"`
				Limit  int             `json:"limit"`
				Offset int             `json:"offset"`
			}
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if string(got.Items) != "[]" || got.Limit != tc.limit || got.Offset != tc.offset || historyCalls != 1 {
				t.Fatalf("history envelope=%+v, calls=%d", got, historyCalls)
			}
		})
	}
}

func TestAvailabilityHistoryReturnsReportedTransitions(t *testing.T) {
	previous := true
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	want := []availability.Change{{ID: 7, AgentID: "agent-1", DeviceID: "device-1", Kind: "event_offline", Source: "remote", MessageID: "message-7", EventID: "event-7", TimeBasis: "observed_at", FromOnline: &previous, ToOnline: false, ObservedAt: now, ReceivedAt: now}}
	repo := availabilityRepository{changesFunc: func(context.Context, string, string, int, int) ([]availability.Change, error) { return want, nil }}
	w := availabilityRequest(repo, "/api/devices/device-1/availability/history")
	var got struct {
		Items []availability.Change `json:"items"`
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Items, want) {
		t.Fatalf("transitions=%+v, want %+v", got.Items, want)
	}
}

func TestAvailabilityErrorsAndQueryBounds(t *testing.T) {
	for _, tc := range []struct {
		path string
		err  error
		code int
	}{
		{"/api/devices/router/availability", storage.ErrNotFound, 404},
		{"/api/devices/router/availability", storage.ErrAmbiguous, 409},
		{"/api/devices/router/availability/history", storage.ErrNotFound, 404},
		{"/api/devices/router/availability/history", storage.ErrAmbiguous, 409},
		{"/api/availability/summary", errors.New("private database details"), 503},
		{"/api/devices/router/availability", errors.New("private database details"), 503},
		{"/api/devices/router/availability/history?limit=0", nil, 400},
		{"/api/devices/router/availability/history?limit=1001", nil, 400},
		{"/api/devices/router/availability/history?limit=x", nil, 400},
		{"/api/devices/router/availability/history?offset=-1", nil, 400},
		{"/api/devices/router/availability/history?offset=1000001", nil, 400},
		{"/api/devices/router/availability/history?offset=x", nil, 400},
		{"/api/availability/summary?agent_id=" + strings.Repeat("a", 257), nil, 400},
		{"/api/devices/router/availability?agent_id=" + strings.Repeat("a", 257), nil, 400},
		{"/api/devices/" + strings.Repeat("a", 257) + "/availability/history", nil, 400},
	} {
		t.Run(tc.path, func(t *testing.T) {
			repo := availabilityRepository{fakeRepository: fakeRepository{failure: tc.err}, changesFunc: func(context.Context, string, string, int, int) ([]availability.Change, error) {
				t.Fatal("history must not be read after invalid query or failed device resolution")
				return nil, nil
			}}
			w := availabilityRequest(repo, tc.path)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "private database details") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	w := availabilityRequest(availabilityRepository{changesFunc: func(context.Context, string, string, int, int) ([]availability.Change, error) {
		return nil, errors.New("private database details")
	}}, "/api/devices/router/availability/history")
	if w.Code != 503 || strings.Contains(w.Body.String(), "private database details") {
		t.Fatalf("history storage failure: status=%d body=%s", w.Code, w.Body.String())
	}
}
