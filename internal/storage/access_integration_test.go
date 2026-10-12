package storage

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixturePassword = "only a test password"

func adminFixture(t *testing.T, s *Store) (*access.Service, access.Principal) {
	t.Helper()
	hash, err := access.HashPassword(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccessCreateUser(t.Context(), access.Principal{}, "owner", "admin", hash, true, access.Meta{RequestID: "fixture", Peer: "test"}); err != nil {
		t.Fatal(err)
	}
	svc := access.New(s, access.Config{Mode: "required", SessionTTL: 8 * time.Hour})
	p, err := svc.Login(t.Context(), "owner", fixturePassword, access.Meta{Peer: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, p
}
func TestAccessBootstrapLoginHashedSessionsAndLogout(t *testing.T) {
	s := integrationStore(t)
	svc, p := adminFixture(t, s)
	ctx := t.Context()
	if _, err := s.AccessCreateUser(ctx, access.Principal{}, "other", "admin", "unused", true, access.Meta{}); err != access.ErrBootstrap {
		t.Fatal("bootstrap overwritten live access")
	}
	stored, err := s.AccessSession(ctx, p.SessionHash)
	if err != nil || stored.User.Role != "admin" {
		t.Fatal("session unavailable")
	}
	var payload string
	if err = s.pool.QueryRow(ctx, `SELECT encode(token_hash,'hex') FROM access_sessions`).Scan(&payload); err != nil || strings.Contains(payload, p.Token) {
		t.Fatal("plaintext session persisted")
	}
	if _, err = svc.Login(ctx, "missing", fixturePassword, access.Meta{Peer: "another"}); err != access.ErrCredentials {
		t.Fatal("missing username exposed")
	}
	if _, err = svc.Login(ctx, "owner", "wrong password", access.Meta{Peer: "another"}); err != access.ErrCredentials {
		t.Fatal("wrong password accepted")
	}
	if err = s.AccessLogout(ctx, p, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccessSession(ctx, p.SessionHash); err != access.ErrCredentials {
		t.Fatal("logout session still works")
	}
}
func TestAccessRolesRevocationDisableAndLastAdmin(t *testing.T) {
	s := integrationStore(t)
	svc, admin := adminFixture(t, s)
	ctx := t.Context()
	hash, err := access.HashPassword(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.AccessCreateUser(ctx, admin, "worker", "operator", hash, false, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := svc.Login(ctx, "worker", fixturePassword, access.Meta{Peer: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	role := "viewer"
	if _, err = s.AccessUpdateUser(ctx, worker, user.ID, &role, nil, access.Meta{}); err != access.ErrForbidden {
		t.Fatal("operator changed roles")
	}
	if _, err = s.AccessUpdateUser(ctx, admin, user.ID, &role, nil, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccessSession(ctx, worker.SessionHash); err != access.ErrCredentials {
		t.Fatal("role change did not revoke session")
	}
	disabled := true
	if _, err = s.AccessUpdateUser(ctx, admin, admin.User.ID, nil, &disabled, access.Meta{}); err != access.ErrLastAdmin {
		t.Fatal("last admin disabled")
	}
	if _, err = s.AccessUpdateUser(ctx, admin, admin.User.ID, &role, nil, access.Meta{}); err != access.ErrLastAdmin {
		t.Fatal("last admin demoted")
	}
	if _, err = s.AccessUpdateUser(ctx, admin, user.ID, nil, &disabled, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Login(ctx, "worker", fixturePassword, access.Meta{Peer: "new-worker"}); err != access.ErrCredentials {
		t.Fatal("disabled user logged in")
	}
}
func TestAccessConcurrentLastAdminProtection(t *testing.T) {
	s := integrationStore(t)
	svc, first := adminFixture(t, s)
	ctx := t.Context()
	hash, err := access.HashPassword(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AccessCreateUser(ctx, first, "second", "admin", hash, false, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	secondP, err := svc.Login(ctx, "second", fixturePassword, access.Meta{Peer: "second"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	role := "viewer"
	for _, p := range []access.Principal{first, secondP} {
		go func(p access.Principal) {
			defer wg.Done()
			_, err := s.AccessUpdateUser(ctx, p, p.User.ID, &role, nil, access.Meta{})
			results <- err
		}(p)
	}
	wg.Wait()
	close(results)
	succeeded, blocked := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if err == access.ErrLastAdmin {
			blocked++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || blocked != 1 {
		t.Fatalf("racing demotions: %d/%d", succeeded, blocked)
	}
	var admins int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_users WHERE role='admin' AND NOT disabled`).Scan(&admins); err != nil || admins != 1 {
		t.Fatal("no surviving admin")
	}
}
func TestAccessExpiryPasswordResetAndSessionBound(t *testing.T) {
	s := integrationStore(t)
	svc, admin := adminFixture(t, s)
	ctx := t.Context()
	r, err := s.AccessLoginRecord(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 23; i++ {
		token, _ := access.NewToken()
		h, _ := access.TokenHash(token)
		if _, err = s.AccessStartSession(ctx, r, h, time.Now().Add(time.Hour), access.Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_sessions`).Scan(&count); err != nil || count != 20 {
		t.Fatal("sessions unbounded")
	}
	// The original session can have been evicted; use a fresh authenticated admin.
	admin, err = svc.Login(ctx, "owner", fixturePassword, access.Meta{Peer: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := access.HashPassword("a new fixture password")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AccessSetPassword(ctx, admin, admin.User.ID, newHash, r.PasswordHash, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccessSession(ctx, admin.SessionHash); err != access.ErrCredentials {
		t.Fatal("password reset retained session")
	}
	if _, err = svc.Login(ctx, "owner", fixturePassword, access.Meta{Peer: "fresh"}); err != access.ErrCredentials {
		t.Fatal("old password accepted")
	}
	changed, err := svc.Login(ctx, "owner", "a new fixture password", access.Meta{Peer: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE access_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, changed.SessionHash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AccessSession(ctx, changed.SessionHash); err != access.ErrCredentials {
		t.Fatal("expired session accepted")
	}
}
func TestAccessMonitoringAuditAtomicityAndRevokedActor(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	actorCtx := access.WithMeta(access.WithPrincipal(ctx, admin), access.Meta{RequestID: "monitor-test", Peer: "test"})
	p := monitoring.Policy{TelemetryIntervalSeconds: 30, InventoryIntervalSeconds: 300, TopologyIntervalSeconds: 300}
	if err := s.UpdateMonitoring(actorCtx, "agent-1", "shared-router", p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit ADD CONSTRAINT reject_fixture_audit CHECK(request_id <> 'fail-test')`); err != nil {
		t.Fatal(err)
	}
	badCtx := access.WithMeta(access.WithPrincipal(ctx, admin), access.Meta{RequestID: "fail-test"})
	p.TelemetryIntervalSeconds = 60
	if err := s.UpdateMonitoring(badCtx, "agent-1", "shared-router", p); err == nil {
		t.Fatal("audit error not fatal")
	}
	d, err := s.Device(ctx, "agent-1", "shared-router")
	if err != nil || d.Monitoring.TelemetryIntervalSeconds != 30 {
		t.Fatal("unaudited mutation committed")
	}
	if err := s.AccessLogout(ctx, admin, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMonitoring(actorCtx, "agent-1", "shared-router", p); err != access.ErrForbidden {
		t.Fatal("revoked actor mutated policy")
	}
}
