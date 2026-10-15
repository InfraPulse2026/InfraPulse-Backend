package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type correlationStub struct {
	CorrelationRepository
	calls int
}

func (s *correlationStub) CorrelatedIncidents(context.Context, correlation.Filter) ([]correlation.Incident, error) {
	s.calls++
	return []correlation.Incident{}, nil
}
func (s *correlationStub) SaveCorrelationService(context.Context, string, correlation.ServiceInput, string) (correlation.Service, bool, error) {
	s.calls++
	return correlation.Service{ID: "svc-1"}, false, nil
}
func (s *correlationStub) RebuildCorrelation(context.Context) (int, error) { s.calls++; return 2, nil }

func TestCorrelationRolesClosedInputAndBounds(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			stub := &correlationStub{}
			auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: role}, SessionHash: hash}}, access.Config{Mode: "required"})
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithCorrelation(stub))
			request := func(method, path, body, key string, auth bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				if auth {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				if key != "" {
					r.Header.Set("Idempotency-Key", key)
				}
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := request("GET", "/api/correlation/incidents", "", "", false); w.Code != 401 {
				t.Fatal("public read", w.Code)
			}
			if w := request("GET", "/api/correlation/incidents", "", "", true); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("read", w.Code)
			}
			if w := request("GET", "/api/correlation/incidents?state=garbage", "", "", true); w.Code != 400 {
				t.Fatal("invalid state", w.Code)
			}
			body := `{"name":"Voice","kind":"voice","enabled":true,"assets":[{"asset_id":"asset-1","role":"provider"}],"expected_revision":0,"confirmed":true,"reason":"reviewed dependency"}`
			want := 403
			if role == "admin" {
				want = 201
			}
			if w := request("POST", "/api/correlation/services", body, "service-key", true); w.Code != want {
				t.Fatalf("create %d %s", w.Code, w.Body.String())
			}
			if role == "admin" {
				before := stub.calls
				for _, b := range []string{`{"confirmed":true,"unexpected":1}`, `{"name":"Voice","kind":"voice","assets":[],"confirmed":true,"reason":"checked"}`} {
					if w := request("POST", "/api/correlation/services", b, "bad", true); w.Code != 400 {
						t.Fatal("bad input", w.Code)
					}
				}
				if stub.calls != before {
					t.Fatal("invalid body reached store")
				}
				if w := request("POST", "/api/correlation/rebuild", `{"confirmed":true}`, "", true); w.Code != 202 {
					t.Fatal("rebuild", w.Code)
				}
			}
		})
	}
}
