package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type rollbackStub struct {
	commandStub
	origin        string
	rollbackCalls int
}

func (f *rollbackStub) CreateRollback(_ context.Context, id string, r commands.RollbackRequest, key string) (commands.Record, bool, error) {
	f.origin = id
	f.rollbackCalls++
	return commands.Record{ID: "cmd_" + strings.Repeat("b", 32), Operation: "rollback"}, f.replay, f.failure
}
func (f *rollbackStub) CommandSnapshot(context.Context, string) (commands.Snapshot, error) {
	return commands.Snapshot{Status: "candidate", ExpiryBasis: "not_transmitted_by_agent"}, f.failure
}
func (f *rollbackStub) Snapshots(context.Context, string, string, int, int) ([]commands.Snapshot, error) {
	return nil, f.failure
}
func TestRollbackHTTPRolesAndTypedOrigin(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		repo := &rollbackStub{}
		token, _ := access.NewToken()
		hash, _ := access.TokenHash(token)
		svc := access.New(accessStub{principal: access.Principal{User: access.User{ID: "usr_" + strings.Repeat("a", 32), Role: role}, SessionHash: hash}}, access.Config{Mode: "required", SessionTTL: time.Hour})
		h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(svc), WithCommands(repo))
		id := "cmd_" + strings.Repeat("a", 32)
		r := httptest.NewRequest("POST", "/api/commands/"+id+"/rollback", strings.NewReader(`{"expected_admin_up":false,"timeout_seconds":30,"confirmed":true}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "undo-once")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 202
		if role == "viewer" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("rollback %s: %d %s", role, w.Code, w.Body.String())
		}
		if role == "viewer" && repo.rollbackCalls > 0 {
			t.Fatal("viewer reached rollback storage")
		}
		if role != "viewer" && repo.origin != id {
			t.Fatal("origin changed in transit")
		}
		w = httptest.NewRecorder()
		r = httptest.NewRequest("GET", "/api/commands/"+id+"/snapshot", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"expires_at":null`) {
			t.Fatal("viewer snapshot metadata or unknown expiry lost")
		}
	}
}
