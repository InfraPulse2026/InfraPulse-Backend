// Package access defines local operator identities, sessions and permissions.
// It is independent of Agent transport authentication.
package access

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

const CookieName = "infrapulse_session"
const DataRead = "data.read"
const MonitoringWrite = "monitoring.write"
const AccessManage = "access.manage"
const CommandWrite = "commands.write"
const IncidentWrite = "incidents.write"
const ReportWrite = "reports.write"
const AnalysisWrite = "analysis.write"

var ErrCredentials = errors.New("invalid credentials or session")
var ErrForbidden = errors.New("permission denied")
var ErrConflict = errors.New("username already exists")
var ErrLastAdmin = errors.New("cannot disable or demote the last active administrator")
var ErrNotFound = errors.New("access resource not found")
var ErrBootstrap = errors.New("an active administrator already exists")
var ErrBusy = errors.New("authentication temporarily rate limited")

type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}
type LoginRecord struct {
	User
	PasswordHash string `json:"-"`
}
type Principal struct {
	User        User      `json:"user"`
	ExpiresAt   time.Time `json:"expires_at"`
	SessionHash []byte    `json:"-"`
	Token       string    `json:"-"`
	Cookie      bool      `json:"-"`
}

func Permissions(role string) []string {
	switch role {
	case "viewer":
		return []string{DataRead}
	case "operator":
		return []string{DataRead, MonitoringWrite, CommandWrite, IncidentWrite, ReportWrite, AnalysisWrite}
	case "admin":
		return []string{DataRead, MonitoringWrite, AccessManage, CommandWrite, IncidentWrite, ReportWrite, AnalysisWrite}
	default:
		return []string{}
	}
}
func Allowed(role, permission string) bool {
	for _, p := range Permissions(role) {
		if p == permission {
			return true
		}
	}
	return false
}
func ValidRole(role string) bool { return role == "viewer" || role == "operator" || role == "admin" }

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func Username(input string) (string, error) {
	s := strings.ToLower(input)
	if !usernamePattern.MatchString(s) {
		return "", errors.New("username must be 3..64 ASCII letters, numbers, dots, underscores or hyphens")
	}
	return s, nil
}

type Meta struct {
	RequestID string
	Peer      string
}
type Audit struct {
	ID        int64     `json:"id"`
	ActorID   *string   `json:"actor_id"`
	TargetID  *string   `json:"target_id"`
	Action    string    `json:"action"`
	Outcome   string    `json:"outcome"`
	Resource  string    `json:"resource"`
	RequestID string    `json:"request_id"`
	Peer      string    `json:"peer"`
	At        time.Time `json:"at"`
}
type Repository interface {
	AccessLoginRecord(context.Context, string) (LoginRecord, error)
	AccessStartSession(context.Context, LoginRecord, []byte, time.Time, Meta) (Principal, error)
	AccessSession(context.Context, []byte) (Principal, error)
	AccessLogout(context.Context, Principal, bool, Meta) error
	AccessLoginFailure(context.Context, Meta) error
	AccessUsers(context.Context, int, int) ([]User, error)
	AccessCreateUser(context.Context, Principal, string, string, string, bool, Meta) (User, error)
	AccessUpdateUser(context.Context, Principal, string, *string, *bool, Meta) (User, error)
	AccessSetPassword(context.Context, Principal, string, string, string, Meta) error
	AccessAudit(context.Context, int, int) ([]Audit, error)
}
type Config struct {
	Mode         string
	SessionTTL   time.Duration
	PublicOrigin string
	CookieSecure bool
}
type contextKey struct{}
type metaKey struct{}

func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}
func CurrentMeta(ctx context.Context) Meta { m, _ := ctx.Value(metaKey{}).(Meta); return m }
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}
func Current(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}
