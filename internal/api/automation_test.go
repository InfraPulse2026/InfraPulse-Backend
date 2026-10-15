package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type automationAPIStub struct{ calls int }

func (s *automationAPIStub) AutomationTarget(context.Context, string) (au.Target, error) {
	s.calls++
	return au.Target{}, nil
}
func (s *automationAPIStub) PutAutomationTarget(context.Context, string, au.TargetInput) (au.Target, error) {
	s.calls++
	return au.Target{}, nil
}
func (s *automationAPIStub) AutomationWorkers(context.Context) ([]au.WorkerStatus, error) {
	s.calls++
	return []au.WorkerStatus{}, nil
}
func (s *automationAPIStub) CreateAutomation(context.Context, string, au.Input, string) (au.Job, bool, error) {
	s.calls++
	return au.Job{ID: "job_one"}, true, nil
}
func (s *automationAPIStub) AutomationJob(context.Context, string) (au.Job, error) {
	s.calls++
	return au.Job{}, nil
}
func (s *automationAPIStub) AutomationJobs(context.Context, string, int, int) ([]au.Job, error) {
	s.calls++
	return []au.Job{}, nil
}
func (s *automationAPIStub) AutomationEvents(context.Context, string, int, int) ([]au.Event, error) {
	s.calls++
	return []au.Event{}, nil
}
func (s *automationAPIStub) CancelAutomation(context.Context, string) (au.Job, error) {
	s.calls++
	return au.Job{}, nil
}
func (s *automationAPIStub) AutomationSnapshots(context.Context, string, int, int) ([]au.Snapshot, error) {
	s.calls++
	return []au.Snapshot{}, nil
}
func (s *automationAPIStub) AutomationSnapshotContent(context.Context, string) (au.Result, error) {
	s.calls++
	return au.Result{}, nil
}
func TestAutomationRoutesRolesAndClosedInput(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &automationAPIStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAutomation(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/automation/workers", "/api/assets/asset_one/automation-target", "/api/automation/snapshots/snapshot_one/content"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("anonymous", w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 403
				if role == "admin" {
					want = 200
				}
				if w.Code != want {
					t.Fatal(path, w.Code)
				}
			}
			body := `{"target_revision":1,"timeout_seconds":30,"reason":"read reviewed configuration"}`
			r := httptest.NewRequest("POST", "/api/assets/asset_one/automation/jobs", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "job-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 202
			if role == "viewer" {
				want = 403
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			if role != "viewer" {
				if w.Header().Get("Idempotent-Replay") != "true" {
					t.Fatal("idempotency header")
				}
				before := repo.calls
				for _, bad := range []string{`{"target_revision":1,"timeout_seconds":30,"reason":"test","command":"reboot"}`, `{"target_revision":1,"timeout_seconds":99999,"reason":"test"}`, body + ` {}`, strings.Repeat("x", 2049)} {
					r = httptest.NewRequest("POST", "/api/assets/asset_one/automation/jobs", strings.NewReader(bad))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "bad-key")
					w = httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 400 || repo.calls != before {
						t.Fatal("bad body reached store", w.Code)
					}
				}
			}
		})
	}
}
