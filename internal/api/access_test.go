package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
)

type accessStub struct {
	access.Repository
	principal access.Principal
	failure   error
}

func (f accessStub) AccessSession(_ context.Context, hash []byte) (access.Principal, error) {
	if f.failure != nil {
		return access.Principal{}, f.failure
	}
	if !bytes.Equal(hash, f.principal.SessionHash) {
		return access.Principal{}, access.ErrCredentials
	}
	return f.principal, nil
}
func authHandler(t *testing.T, role string) (http.Handler, string) {
	t.Helper()
	token, err := access.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := access.TokenHash(token)
	stub := accessStub{principal: access.Principal{User: access.User{ID: "usr_" + strings.Repeat("a", 32), Username: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
	svc := access.New(stub, access.Config{Mode: "required", SessionTTL: time.Hour})
	return New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(svc)), token
}
func TestAllDeclaredDataRoutesRequireAuthentication(t *testing.T) {
	h, _ := authHandler(t, "viewer")
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openapi, &spec); err != nil {
		t.Fatal(err)
	}
	for path, operations := range spec.Paths {
		if !strings.HasPrefix(path, "/api/") || path == "/api/auth/login" || path == "/api/auth/token" {
			continue
		}
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{id}", "fixture"), "{key}", "fixture")
		for method := range operations {
			if method != "get" && method != "put" && method != "post" && method != "patch" {
				continue
			}
			t.Run(method+path, func(t *testing.T) {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(strings.ToUpper(method), path, nil))
				if w.Code != 401 {
					t.Fatalf("unprotected route %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}
func TestRolesHeadersAndDefaultDeny(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		h, token := authHandler(t, role)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/devices", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal("authenticated read denied")
		}
		body := `{"telemetry_interval_seconds":3,"inventory_interval_seconds":300,"topology_interval_seconds":300}`
		w = httptest.NewRecorder()
		r = httptest.NewRequest("PUT", "/api/devices/device-1/monitoring", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Role", "admin")
		r.Header.Set("X-User", "owner")
		h.ServeHTTP(w, r)
		want := 200
		if role == "viewer" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("role %s status %d", role, w.Code)
		}
		if role != "admin" {
			w = httptest.NewRecorder()
			r = httptest.NewRequest("GET", "/api/access/users", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal("access management privilege escalated")
			}
		}
	}
	w := httptest.NewRecorder()
	New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/devices", nil))
	if w.Code != 503 {
		t.Fatal("unconfigured API failed open")
	}
}
func TestCookieCSRFOriginAndCredentialAmbiguity(t *testing.T) {
	h, token := authHandler(t, "operator")
	body := `{"expected_interval_seconds":30}`
	for _, c := range []struct {
		csrf, origin string
		want         int
	}{{"", "", 403}, {access.CSRF(token), "http://example.com", 200}, {access.CSRF(token), "https://attacker.test", 403}, {"wrong", "http://example.com", 403}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/api/telephony/targets/pbx/monitoring", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
		if c.csrf != "" {
			r.Header.Set("X-CSRF-Token", c.csrf)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatalf("CSRF/origin case %d %s", w.Code, w.Body.String())
		}
	}
	for _, kind := range []string{"mixed", "duplicate", "query", "invalid"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/devices", nil)
		switch kind {
		case "mixed":
			r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
			r.Header.Set("Authorization", "Bearer "+token)
		case "duplicate":
			r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
			r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
		case "query":
			r.URL.RawQuery = "access_token=" + token
		case "invalid":
			r.Header.Set("Authorization", "Bearer invalid")
		}
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("ambiguous or query credential accepted")
		}
	}
}
func TestAccessStorageFailureDoesNotLeakDetails(t *testing.T) {
	token, _ := access.NewToken()
	svc := access.New(accessStub{failure: errors.New("private DB secret")}, access.Config{Mode: "required"})
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(svc))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/devices", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private DB secret") {
		t.Fatal("failed-open storage or leaked diagnostics")
	}
}
