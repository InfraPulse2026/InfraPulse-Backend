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

type sourceStub struct{ calls int }

func (s *sourceStub) DataSources(context.Context, int, int) ([]datasources.Source, error) {
	s.calls++
	return []datasources.Source{}, nil
}
func (s *sourceStub) DataSource(context.Context, string) (datasources.Source, error) {
	s.calls++
	return datasources.Source{ID: "fixture"}, nil
}
func (s *sourceStub) SaveDataSource(_ context.Context, _ string, in datasources.Input) (datasources.Source, error) {
	s.calls++
	_, err := in.Normalize()
	return datasources.Source{ID: "fixture", HasCredential: in.Credential != nil}, err
}

func TestDataSourceHTTPAuthorizationAndSecretResponse(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			stub := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &sourceStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(stub, access.Config{Mode: "required", SessionTTL: time.Hour})), WithDataSources(repo))
			for _, route := range []string{"/api/data-sources", "/api/data-sources/fixture", "/api/data-sources/fixture/status"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", route, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("source read denied", role, w.Code)
				}
			}
			before := repo.calls
			body := `{"type":"ZABBIX","name":"PROD","endpoint":"https://monitor.example","enabled":true,"sync_interval_seconds":3,"credential":"private-fixture-token"}`
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/api/data-sources", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(w, r)
			want := 403
			if role == "admin" {
				want = 201
			}
			if w.Code != want || strings.Contains(w.Body.String(), "private-fixture-token") {
				t.Fatal("source mutation authorization/secret leak", w.Code, w.Body.String())
			}
			if role != "admin" && repo.calls != before {
				t.Fatal("forbidden request reached storage")
			}
			if role == "admin" {
				w = httptest.NewRecorder()
				r = httptest.NewRequest("POST", "/api/data-sources", strings.NewReader(`{"type":"ZABBIX","name":"fixture","endpoint":"https://monitor.example","enabled":true,"sync_interval_seconds":3,"configuration":{"insecure_skip_verify":true}}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				if w.Code != 400 {
					t.Fatal("REST accepted TLS bypass", w.Code)
				}
			}
		})
	}
}
