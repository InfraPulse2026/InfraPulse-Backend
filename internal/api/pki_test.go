package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
)

type pkiAdminStub struct {
	pki.Repository
	created int
}

func (s *pkiAdminStub) PKICreateToken(ctx context.Context, token pki.Token) error {
	p, ok := access.Current(ctx)
	if !ok || p.User.Role != "admin" {
		return access.ErrForbidden
	}
	s.created++
	return nil
}
func TestPKIManagementRolesAndCookieCSRF(t *testing.T) {
	for _, role := range []string{"viewer", "operator"} {
		handler, token := authHandler(t, role)
		for _, path := range []string{"/api/pki/bootstrap-tokens", "/api/pki/certificates", "/api/pki/audit"} {
			for _, method := range []string{"GET", "POST"} {
				if method == "POST" && path != "/api/pki/bootstrap-tokens" {
					continue
				}
				req := httptest.NewRequest(method, path, strings.NewReader(`{"agent_id":"bf6e7560-9b04-4d28-ae98-5dfe7d4f2001"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 403 {
					t.Fatalf("role=%s method=%s path=%s status=%d", role, method, path, rec.Code)
				}
			}
		}
	}
	token, err := access.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := access.TokenHash(token)
	auth := access.New(accessStub{principal: access.Principal{User: access.User{ID: "usr_" + strings.Repeat("a", 32), Role: "admin"}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}, access.Config{Mode: "required", SessionTTL: time.Hour})
	repo := &pkiAdminStub{}
	handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithPKI(&pki.Service{Repository: repo}))
	body := `{"agent_id":"bf6e7560-9b04-4d28-ae98-5dfe7d4f2001","ttl_seconds":900}`
	for _, csrf := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/api/pki/bootstrap-tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "infrapulse_session", Value: token})
		if csrf {
			req.Header.Set("X-CSRF-Token", access.CSRF(token))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !csrf {
			if rec.Code != 403 || repo.created != 0 {
				t.Fatal("cookie mutation missing CSRF accepted")
			}
			continue
		}
		if rec.Code != 201 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("admin token create status=%d", rec.Code)
		}
		var result map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &result) != nil || result["token"] == nil || result["Hash"] != nil || result["hash"] != nil {
			t.Fatal("token response incorrect")
		}
	}
}
