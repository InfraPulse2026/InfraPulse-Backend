package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

type sourceJobStub struct {
	sourceStub
	calls   int
	replay  bool
	failure error
}

func (s *sourceJobStub) QueueDataSourceJob(ctx context.Context, id, op, key string) (datasources.Job, bool, error) {
	s.calls++
	actor, ok := access.Current(ctx)
	if !ok || actor.User.Role != "admin" {
		return datasources.Job{}, false, access.ErrForbidden
	}
	return datasources.Job{ID: "job-fixture", SourceID: id, Operation: op, State: "queued", LeaseToken: "private-lease-token"}, s.replay, s.failure
}
func (s *sourceJobStub) DataSourceJobs(context.Context, string, int, int) ([]datasources.Job, error) {
	return []datasources.Job{}, nil
}
func (s *sourceJobStub) DataSourceJob(context.Context, string, string) (datasources.Job, error) {
	return datasources.Job{State: "queued"}, nil
}
func (s *sourceJobStub) DataSourceJobEvents(context.Context, string, string) ([]datasources.Event, error) {
	return []datasources.Event{}, nil
}

func TestSourceJobHTTPAuthorizationKeysReplayAndUnsupported(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			stub := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &sourceJobStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(stub, access.Config{Mode: "required", SessionTTL: time.Hour})), WithDataSources(repo))
			for _, path := range []string{"/api/data-sources/source/jobs", "/api/data-sources/source/jobs/job-fixture", "/api/data-sources/source/jobs/job-fixture/events"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("history read denied", w.Code)
				}
			}
			for _, op := range []string{"test", "sync"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/api/data-sources/source/"+op, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Idempotency-Key", "job-key")
				h.ServeHTTP(w, r)
				want := 403
				if role == "admin" {
					want = 202
				}
				if w.Code != want || strings.Contains(w.Body.String(), "private-lease-token") {
					t.Fatal("queue access or lease leak", w.Code, w.Body.String())
				}
			}
			if role != "admin" {
				if repo.calls != 0 {
					t.Fatal("forbidden operation reached queue")
				}
				return
			}
			for _, keys := range [][]string{nil, {"first", "second"}, {strings.Repeat("x", 129)}} {
				before := repo.calls
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/api/data-sources/source/test", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				for _, key := range keys {
					r.Header.Add("Idempotency-Key", key)
				}
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.calls != before {
					t.Fatal("invalid keys accepted", w.Code)
				}
			}
			repo.replay = true
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/api/data-sources/source/test", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", "job-key")
			h.ServeHTTP(w, r)
			if w.Code != 202 || w.Header().Get("Idempotent-Replay") != "true" {
				t.Fatal("replay contract", w.Code)
			}
			repo.failure = datasources.ErrUnsupported
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "operation_unsupported") {
				t.Fatal("unsupported operation not explicit", w.Code)
			}
		})
	}
}
