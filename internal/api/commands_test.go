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
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
)

type commandStub struct {
	failure error
	replay  bool
	calls   int
	request commands.Request
}

func (f *commandStub) CreateCommand(_ context.Context, r commands.Request, key string) (commands.Record, bool, error) {
	f.calls++
	f.request = r
	return commands.Record{ID: "cmd_" + strings.Repeat("a", 32), State: "queued", IdempotencyKey: key}, f.replay, f.failure
}
func (f *commandStub) Command(context.Context, string) (commands.Record, error) {
	return commands.Record{}, f.failure
}
func (f *commandStub) Commands(context.Context, commands.Filter) ([]commands.Record, error) {
	return nil, f.failure
}
func (f *commandStub) CommandEvents(context.Context, string) ([]commands.Event, error) {
	return []commands.Event{}, f.failure
}
func (f *commandStub) CommandPolicy() commands.Policy { return commands.Defaults() }
func TestCommandCreateAuthValidationAndReplay(t *testing.T) {
	body := `{"agent_id":"agent-1","device_id":"router","interface_id":"ether2","operation":"enable","expected_admin_up":false,"timeout_seconds":10,"confirmed":true}`
	for _, role := range []string{"viewer", "operator", "admin"} {
		token, _ := access.NewToken()
		hash, _ := access.TokenHash(token)
		repo := &commandStub{}
		svc := access.New(accessStub{principal: access.Principal{User: access.User{ID: "usr_" + strings.Repeat("a", 32), Username: "owner", Role: role}, SessionHash: hash}}, access.Config{Mode: "required", SessionTTL: time.Hour})
		h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(svc), WithCommands(repo))
		for _, replay := range []bool{false, true} {
			repo.replay = replay
			r := httptest.NewRequest("POST", "/api/commands", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", "request-1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 202
			if replay {
				want = 200
			}
			if role == "viewer" {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s/%t %d %s", role, replay, w.Code, w.Body.String())
			}
		}
		if role == "viewer" && repo.calls != 0 {
			t.Fatal("viewer reached command creation")
		}
		if role != "viewer" && (repo.request.ExpectedAdminUp == nil || *repo.request.ExpectedAdminUp) {
			t.Fatal("false precondition replaced with missing value")
		}
		for _, bad := range []string{strings.Replace(body, `"confirmed":true`, `"confirmed":false`, 1), strings.Replace(body, `"operation":"enable"`, `"operation":"shell.exec"`, 1), strings.Replace(body, `"timeout_seconds":10`, `"timeout_seconds":61`, 1)} {
			if role == "viewer" {
				continue
			}
			r := httptest.NewRequest("POST", "/api/commands", strings.NewReader(bad))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", "request-2")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatal("invalid command accepted")
			}
		}
	}
}
func TestCommandErrorsAndDisabledMode(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{{commands.ErrBusy, 409}, {commands.ErrConflict, 409}, {commands.ErrPrecondition, 409}, {commands.ErrProtected, 403}, {commands.ErrDisabled, 403}, {commands.ErrNotFound, 404}} {
		a := &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		w := httptest.NewRecorder()
		a.commandFail(w, c.err)
		if w.Code != c.code {
			t.Fatal("incorrect command status")
		}
	}
	repo := &commandStub{}
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDevelopmentAuthDisabled(), WithCommands(repo))
	r := httptest.NewRequest("POST", "/api/commands", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || repo.calls != 0 {
		t.Fatal("command allowed without user identity")
	}
}
