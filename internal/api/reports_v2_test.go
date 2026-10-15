package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
)

type reportV2Stub struct {
	reportStub
	ReportV2Repository
	saved int
}

func (s *reportV2Stub) SaveReportSchedule(_ context.Context, _ string, v reports.ScheduleInput) (reports.Schedule, error) {
	s.saved++
	return reports.Schedule{ID: "schedule-fixture", Definition: v}, nil
}

func (s *reportV2Stub) ReportSchedules(context.Context, int, int) ([]reports.Schedule, error) {
	return []reports.Schedule{}, nil
}

func TestReportV2ScheduleAuthorizationAndClosedJSON(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, err := access.NewToken()
			if err != nil {
				t.Fatal(err)
			}
			hash, _ := access.TokenHash(token)
			auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
			repo := &reportV2Stub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithReports(repo))
			body := `{"name":"Daily","enabled":true,"cadence":"daily","confirmed":true,"reason":"daily review"}`
			for _, tc := range []struct {
				method, body string
				cookie, csrf bool
				want         int
			}{
				{"GET", "", false, false, 200},
				{"POST", body, false, false, 201},
				{"POST", body, true, false, 403},
				{"POST", body, true, true, 201},
				{"POST", strings.TrimSuffix(body, "}") + `,"unknown":true}`, false, false, 400},
			} {
				req := httptest.NewRequest(tc.method, "/api/reports/schedules", strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				if tc.cookie {
					req.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
				} else {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				if tc.csrf {
					req.Header.Set("X-CSRF-Token", access.CSRF(token))
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				want := tc.want
				if role != "admin" {
					want = 403
				}
				if rec.Code != want {
					t.Fatalf("%s cookie=%v csrf=%v: got %d want %d body=%s", tc.method, tc.cookie, tc.csrf, rec.Code, want, rec.Body.String())
				}
			}
			if role != "admin" && repo.saved != 0 {
				t.Fatal("unauthorized schedule reached storage")
			}
		})
	}
}
