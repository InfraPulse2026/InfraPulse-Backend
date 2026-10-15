package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/diagnosis"
)

type diagnosisStub struct{ IncidentRepository }

func (diagnosisStub) IncidentContext(context.Context, string, int, int) (diagnosis.Context, error) {
	return diagnosis.Context{Version: "1", Related: []diagnosis.Related{}, Limitations: []string{"no causal claim"}}, nil
}
func TestDiagnosticContextProtectedReadAndBounds(t *testing.T) {
	token, err := access.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := access.TokenHash(token)
	auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: "viewer"}, SessionHash: hash}}, access.Config{Mode: "required"})
	handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithIncidents(diagnosisStub{}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/seed/context", nil))
	if rec.Code != 401 {
		t.Fatal("public diagnostic context")
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/api/incidents/seed/context", 200}, {"/api/incidents/seed/context?limit=51", 400}, {"/api/incidents/seed/context?window_seconds=59", 400}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s status=%d", tc.path, rec.Code)
		}
	}
}
