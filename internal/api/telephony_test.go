package api

import (
	"context"
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type sipRepository struct {
	fakeRepository
	historyFilter telephony.HistoryFilter
	filter        telephony.Filter
	policy        int
}

func (f *sipRepository) TelephonyHistory(_ context.Context, v telephony.HistoryFilter) (telephony.HistoryPage, error) {
	f.historyFilter = v
	return telephony.HistoryPage{Limit: v.Limit}, f.failure
}
func (f *sipRepository) TelephonyTargets(_ context.Context, v telephony.Filter) ([]telephony.Target, error) {
	f.filter = v
	return nil, f.failure
}
func (f *sipRepository) UpdateTelephonyMonitoring(_ context.Context, agent, target string, seconds int) error {
	f.policy = seconds
	return f.failure
}

func TestSIPAPIRoutesAndEmptyLists(t *testing.T) {
	repo := &sipRepository{}
	h := testAPI(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, path := range []string{"/api/telephony/targets?transport=tls&source=sip.options&agent_id=agent-1", "/api/telephony/summary", "/api/telephony/targets/pbx", "/api/telephony/targets/pbx/monitoring", "/api/telephony/targets/pbx/history?from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z&before_id=9&limit=500"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(path, "history") || strings.Contains(path, "targets?") {
			var v map[string]json.RawMessage
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			if string(v["items"]) != "[]" {
				t.Fatal("empty list is not []")
			}
		}
	}
	if repo.filter.Transport != "tls" || repo.filter.AgentID != "agent-1" || repo.historyFilter.AgentID != "agent-1" || repo.historyFilter.TargetID != "pbx" || repo.historyFilter.BeforeID != 9 || repo.historyFilter.Limit != 500 {
		t.Fatal("filters or target resolution lost")
	}
}
func TestSIPAPIRejectsInvalidQueries(t *testing.T) {
	for _, path := range []string{"/api/telephony/targets?transport=http", "/api/telephony/targets?limit=501", "/api/telephony/targets?offset=-1", "/api/telephony/targets/pbx/history?before_id=0", "/api/telephony/targets/pbx/history?from=bad", "/api/telephony/targets/pbx/history?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z", "/api/telephony/summary?agent_id=" + strings.Repeat("a", 257)} {
		w := httptest.NewRecorder()
		testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatalf("%s accepted", path)
		}
	}
}
func TestSIPMonitoringBoundsAndManualApplication(t *testing.T) {
	for _, body := range []string{`{"expected_interval_seconds":3}`, `{"expected_interval_seconds":86400}`} {
		repo := &sipRepository{}
		w := httptest.NewRecorder()
		testAPI(repo, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("PUT", "/api/telephony/targets/pbx/monitoring", strings.NewReader(body)))
		if w.Code != 200 || repo.policy == 0 || !strings.Contains(w.Body.String(), `"application_status":"unverified"`) || !strings.Contains(w.Body.String(), `"delivery_mode":"manual_agent_yaml"`) {
			t.Fatalf("settings lie about delivery: %d %s", w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{}`, `null`, `{"expected_interval_seconds":2}`, `{"expected_interval_seconds":86401}`, `{"expected_interval_seconds":3.5}`, `{"expected_interval_seconds":3,"extra":true}`, `{"expected_interval_seconds":3} {}`} {
		w := httptest.NewRecorder()
		testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("PUT", "/api/telephony/targets/pbx/monitoring", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal("invalid SIP policy accepted")
		}
	}
}
func TestSIPAmbiguityAndNotFoundErrors(t *testing.T) {
	for _, c := range []struct {
		err       error
		code      int
		errorCode string
	}{{storage.ErrAmbiguous, 409, "ambiguous_target"}, {storage.ErrNotFound, 404, "not_found"}} {
		w := httptest.NewRecorder()
		testAPI(fakeRepository{failure: c.err}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/telephony/targets/pbx", nil))
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.errorCode) {
			t.Fatalf("wrong target error %d %s", w.Code, w.Body.String())
		}
	}
}
