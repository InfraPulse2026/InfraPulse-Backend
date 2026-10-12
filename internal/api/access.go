package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Option func(*API)

func WithAccess(s *access.Service) Option { return func(a *API) { a.auth = s } }

// Explicit bypass for isolated tests and loopback development, never New's default.
func WithDevelopmentAuthDisabled() Option { return func(a *API) { a.development = true } }
func requestMeta(r *http.Request) access.Meta {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = "unknown"
	}
	return access.Meta{RequestID: hex.EncodeToString(b), Peer: peer}
}
func (a *API) accessFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrCredentials):
		w.Header().Set("WWW-Authenticate", `Bearer realm="InfraPulse"`)
		problem(w, 401, "unauthorized", "invalid credentials or expired session")
	case errors.Is(err, access.ErrForbidden):
		problem(w, 403, "forbidden", "permission denied")
	case errors.Is(err, access.ErrConflict):
		problem(w, 409, "username_conflict", "username already exists")
	case errors.Is(err, access.ErrLastAdmin):
		problem(w, 409, "last_administrator", "cannot disable or demote the last active administrator")
	case errors.Is(err, access.ErrNotFound):
		problem(w, 404, "not_found", "user not found")
	case errors.Is(err, access.ErrBusy):
		w.Header().Set("Retry-After", "60")
		problem(w, 429, "rate_limited", "try again later")
	default:
		a.log.Error("access storage request failed", "error", err)
		problem(w, 503, "storage_unavailable", "access service unavailable")
	}
}
func (a *API) originAllowed(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	expected := a.auth.Config.PublicOrigin
	if expected == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		expected = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(origins[0])
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(origins[0], expected)
}
func (a *API) authorize(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		w.Header().Add("Vary", "Cookie")
		if a.development {
			next(w, r)
			return
		}
		if a.auth == nil {
			problem(w, 503, "auth_unavailable", "authentication is not configured")
			return
		}
		if !a.accessTransport(w, r) {
			return
		}
		p, err := a.auth.Authenticate(r.Context(), r)
		if err != nil {
			a.accessFail(w, err)
			return
		}
		if permission != "" && !access.Allowed(p.User.Role, permission) {
			a.accessFail(w, access.ErrForbidden)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !a.originAllowed(r) {
				problem(w, 403, "invalid_origin", "request origin is not allowed")
				return
			}
			if p.Cookie && (len(r.Header.Values("X-CSRF-Token")) != 1 || !access.EqualCSRF(r.Header.Get("X-CSRF-Token"), access.CSRF(p.Token))) {
				problem(w, 403, "csrf_failed", "a valid X-CSRF-Token is required")
				return
			}
		}
		meta := requestMeta(r)
		w.Header().Set("X-Request-ID", meta.RequestID)
		ctx := access.WithMeta(access.WithPrincipal(r.Context(), p), meta)
		next(w, r.WithContext(ctx))
	}
}
func accessJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	return boundedJSON(w, r, dest, 8192)
}
func boundedJSON(w http.ResponseWriter, r *http.Request, dest any, maximum int64) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		problem(w, 415, "unsupported_media_type", "use application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(dest); err != nil {
		problem(w, 400, "invalid_request", "invalid JSON fields")
		return false
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		problem(w, 400, "invalid_request", "expected exactly one JSON object")
		return false
	}
	return true
}
func (a *API) authEnabled(w http.ResponseWriter) bool {
	if a.auth == nil || a.development {
		problem(w, 503, "auth_disabled", "user access management is unavailable in development bypass mode")
		return false
	}
	return true
}

func (a *API) accessTransport(w http.ResponseWriter, r *http.Request) bool {
	if a.auth.Config.CookieSecure && r.TLS == nil && !strings.HasPrefix(a.auth.Config.PublicOrigin, "https://") {
		problem(w, 503, "secure_transport_required", "configure HTTPS or an explicit loopback development setup")
		return false
	}
	return true
}
func (a *API) cookie(w http.ResponseWriter, token string, expires time.Time) {
	age := int(time.Until(expires) / time.Second)
	if token == "" {
		age = -1
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: access.CookieName, Value: token, Path: "/", HttpOnly: true, Secure: a.auth.Config.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: age})
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.authEnabled(w) {
		return
	}
	if !a.accessTransport(w, r) {
		return
	}
	if !a.originAllowed(r) {
		problem(w, 403, "invalid_origin", "request origin is not allowed")
		return
	}
	if r.Header.Get("Authorization") != "" {
		problem(w, 400, "invalid_request", "login requires no Authorization header")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !accessJSON(w, r, &body) {
		return
	}
	p, err := a.auth.Login(r.Context(), body.Username, body.Password, requestMeta(r))
	if err != nil {
		a.accessFail(w, err)
		return
	}
	if r.Pattern == "POST /api/auth/token" {
		write(w, 200, map[string]any{"access_token": p.Token, "token_type": "Bearer", "expires_at": p.ExpiresAt, "user": p.User})
		return
	}
	a.cookie(w, p.Token, p.ExpiresAt)
	write(w, 200, map[string]any{"user": p.User, "expires_at": p.ExpiresAt, "csrf_token": access.CSRF(p.Token)})
}
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	p, _ := access.Current(r.Context())
	write(w, 200, map[string]any{"user": p.User, "permissions": access.Permissions(p.User.Role), "expires_at": p.ExpiresAt, "csrf_token": access.CSRF(p.Token)})
}
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	p, _ := access.Current(r.Context())
	all := r.Pattern == "POST /api/auth/logout-all"
	if err := a.auth.Repo.AccessLogout(r.Context(), p, all, access.CurrentMeta(r.Context())); err != nil {
		a.accessFail(w, err)
		return
	}
	if p.Cookie {
		a.cookie(w, "", time.Time{})
	}
	w.WriteHeader(204)
}
func (a *API) accessUsers(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	if r.Method == "GET" {
		limit, err := number(r, "limit", 100, 1, 500)
		if err != nil {
			problem(w, 400, "invalid_query", "limit must be 1..500")
			return
		}
		offset, err := number(r, "offset", 0, 0, 1000000)
		if err != nil {
			problem(w, 400, "invalid_query", "invalid offset")
			return
		}
		users, err := a.auth.Repo.AccessUsers(r.Context(), limit, offset)
		if err != nil {
			a.accessFail(w, err)
			return
		}
		if users == nil {
			users = []access.User{}
		}
		write(w, 200, map[string]any{"items": users, "limit": limit, "offset": offset})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !accessJSON(w, r, &body) {
		return
	}
	name, err := access.Username(body.Username)
	if err != nil {
		problem(w, 400, "invalid_request", err.Error())
		return
	}
	if !access.ValidRole(body.Role) {
		problem(w, 400, "invalid_request", "role must be viewer, operator or admin")
		return
	}
	hash, err := access.HashPassword(body.Password)
	if err != nil {
		if errors.Is(err, access.ErrBusy) {
			a.accessFail(w, err)
		} else {
			problem(w, 400, "invalid_request", err.Error())
		}
		return
	}
	p, _ := access.Current(r.Context())
	u, err := a.auth.Repo.AccessCreateUser(r.Context(), p, name, body.Role, hash, false, access.CurrentMeta(r.Context()))
	if err != nil {
		a.accessFail(w, err)
		return
	}
	write(w, 201, u)
}

var accessID = regexp.MustCompile(`^usr_[a-f0-9]{32}$`)

func (a *API) accessUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	id := r.PathValue("id")
	if !accessID.MatchString(id) {
		problem(w, 400, "invalid_request", "invalid user ID")
		return
	}
	var body struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if !accessJSON(w, r, &body) {
		return
	}
	if body.Role == nil && body.Disabled == nil || body.Role != nil && !access.ValidRole(*body.Role) {
		problem(w, 400, "invalid_request", "supply a valid role or disabled flag")
		return
	}
	p, _ := access.Current(r.Context())
	u, err := a.auth.Repo.AccessUpdateUser(r.Context(), p, id, body.Role, body.Disabled, access.CurrentMeta(r.Context()))
	if err != nil {
		a.accessFail(w, err)
		return
	}
	write(w, 200, u)
}
func (a *API) accessPassword(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	p, _ := access.Current(r.Context())
	id := r.PathValue("id")
	self := r.Pattern == "POST /api/auth/password"
	if self {
		id = p.User.ID
	}
	if !accessID.MatchString(id) {
		problem(w, 400, "invalid_request", "invalid user ID")
		return
	}
	var body struct {
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if !accessJSON(w, r, &body) {
		return
	}
	oldHash := ""
	if self {
		record, err := a.auth.VerifyCurrentPassword(r.Context(), p, body.CurrentPassword, access.CurrentMeta(r.Context()))
		if err != nil {
			a.accessFail(w, err)
			return
		}
		oldHash = record.PasswordHash
	} else if body.CurrentPassword != "" {
		problem(w, 400, "invalid_request", "current_password is only for changing your own password")
		return
	}
	hash, err := access.HashPassword(body.Password)
	if err != nil {
		if errors.Is(err, access.ErrBusy) {
			a.accessFail(w, err)
		} else {
			problem(w, 400, "invalid_request", err.Error())
		}
		return
	}
	if err = a.auth.Repo.AccessSetPassword(r.Context(), p, id, hash, oldHash, access.CurrentMeta(r.Context())); err != nil {
		a.accessFail(w, err)
		return
	}
	if self && p.Cookie {
		a.cookie(w, "", time.Time{})
	}
	w.WriteHeader(204)
}
func (a *API) accessAudit(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	limit, err := number(r, "limit", 100, 1, 500)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid limit")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid offset")
		return
	}
	items, err := a.auth.Repo.AccessAudit(r.Context(), limit, offset)
	if err != nil {
		a.accessFail(w, err)
		return
	}
	if items == nil {
		items = []access.Audit{}
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
