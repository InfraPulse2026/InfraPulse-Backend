package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

type loggingRepository struct {
	fakeRepository
	filter logbook.Filter
}

func TestLogbookAuditGroupEmptyContinuation(t *testing.T) {
	repo := &loggingRepository{}
	h := testAPI(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	key := "audit:" + strings.Repeat("a", 64)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/audit/events/"+key+"?before_id=1", nil))
	var page logbook.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || page.Items == nil || repo.filter.EventKey != key || !repo.filter.AuditOnly {
		t.Fatalf("group continuation: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/audit/events/"+key, nil))
	if w.Code != 404 {
		t.Fatal("missing group has no 404")
	}
}

func (f *loggingRepository) Logs(_ context.Context, v logbook.Filter) (logbook.Page, error) {
	f.filter = v
	return logbook.Page{Items: []logbook.Record{}, Limit: v.Limit}, nil
}
func TestLogbookFilteringAndCursor(t *testing.T) {
	repo := &loggingRepository{}
	h := testAPI(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/devices/device-1/audit?uid=0&success=false&severity=0&before_id=99&limit=500", nil))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f := repo.filter
	if f.UID == nil || *f.UID != 0 || f.Success == nil || *f.Success || f.Severity == nil || *f.Severity != 0 || !f.AuditOnly || f.AgentID != "agent-1" || f.DeviceID != "device-1" || f.BeforeID != 99 || f.Limit != 500 {
		t.Fatalf("filters lost: %+v", f)
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	if string(data["items"]) != "[]" || string(data["next_before_id"]) != "null" {
		t.Fatal("empty cursor page contract broken")
	}
}

func TestLogbookQueryBoundsAndRoutes(t *testing.T) {
	for _, c := range []struct {
		path string
		code int
	}{
		{"/api/logs", 200}, {"/api/audit", 200}, {"/api/audit/events", 200}, {"/api/logs/summary", 200},
		{"/api/logs?severity=unknown&success=unknown", 200},
		{"/api/logs?uid=4294967296", 400}, {"/api/logs?before_id=-1", 400}, {"/api/logs?severity=8", 400}, {"/api/logs?success=yes", 400},
		{"/api/logs?contains=partial", 400}, {"/api/logs?from=no-date", 400},
		{"/api/logs?contains=error&from=2026-10-01T00:00:00Z&to=2026-10-20T00:00:00Z", 400},
		{"/api/logs?contains=error&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z", 200},
		{"/api/logs?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z", 400},
		{"/api/logs/0", 400}, {"/api/audit/events/invalid", 400},
	} {
		t.Run(c.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
			if w.Code != c.code {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLogbookStorageFailuresAndDeviceAmbiguity(t *testing.T) {
	for _, err := range []error{storage.ErrNotFound, storage.ErrAmbiguous} {
		w := httptest.NewRecorder()
		testAPI(fakeRepository{failure: err}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/devices/router/logs", nil))
		if err == storage.ErrNotFound && w.Code != 404 || err == storage.ErrAmbiguous && w.Code != 409 {
			t.Fatalf("wrong scoped error: %d", w.Code)
		}
	}
}
