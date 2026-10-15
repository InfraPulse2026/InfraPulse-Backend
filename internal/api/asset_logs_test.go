package api

import (
	"context"
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetlogs"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type assetLogHTTPStub struct {
	assetStub
	fakeRepository
	calls int
}

func (s *assetLogHTTPStub) Logs(context.Context, logbook.Filter) (logbook.Page, error) {
	return logbook.Page{Items: []logbook.Record{unsafeLog()}}, nil
}
func (s *assetLogHTTPStub) Log(context.Context, int64, string) (logbook.Record, error) {
	return unsafeLog(), nil
}
func unsafeLog() logbook.Record {
	return logbook.Record{ID: 1, Raw: "privileged-retained-text", Message: "public-event", Syslog: json.RawMessage(`{"transport":"tls","raw_payload":"opaque-octets"}`)}
}
func (s *assetLogHTTPStub) LogRaw(context.Context, int64, string) (assetlogs.Raw, error) {
	s.calls++
	return assetlogs.Raw{Raw: "privileged-retained-text"}, nil
}
func (s *assetLogHTTPStub) AssetLogs(context.Context, string, assetlogs.Filter) (assetlogs.Page, error) {
	s.calls++
	return assetlogs.Page{Items: []assetlogs.Record{assetlogs.Public(unsafeLog(), "asset", "binding", "source", "")}}, nil
}
func (s *assetLogHTTPStub) AssetLog(context.Context, string, int64) (assetlogs.Record, error) {
	s.calls++
	return assetlogs.Public(unsafeLog(), "asset", "binding", "source", ""), nil
}
func (s *assetLogHTTPStub) AssetLogRaw(ctx context.Context, _ string, id int64) (assetlogs.Raw, error) {
	return s.LogRaw(ctx, id, "")
}
func (s *assetLogHTTPStub) AssetAuditEvents(context.Context, string, assetlogs.Filter) (assetlogs.AuditPage, error) {
	s.calls++
	return assetlogs.AuditPage{Items: []assetlogs.AuditEvent{}}, nil
}
func TestAssetLogRoutesPrivilegesAndLegacyRawBypass(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &assetLogHTTPStub{}
			h := New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAssets(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/logs", "/api/logs/1", "/api/audit/events/audit:" + strings.Repeat("a", 64), "/api/assets/asset/logs", "/api/assets/asset/logs/1", "/api/assets/asset/audit/events"} {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 || strings.Contains(w.Body.String(), "privileged-retained-text") || strings.Contains(w.Body.String(), "opaque-octets") {
					t.Fatalf("public/legacy raw bypass %s: %d %s", path, w.Code, w.Body.String())
				}
			}
			for _, path := range []string{"/api/logs/1/raw", "/api/assets/asset/logs/1/raw"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("anonymous raw read", w.Code)
				}
				r = httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 403
				if role == "admin" {
					want = 200
				}
				if w.Code != want {
					t.Fatalf("raw role %s: %d %s", role, w.Code, w.Body.String())
				}
			}
			for _, path := range []string{"/api/assets/asset/logs?raw=true", "/api/assets/asset/logs?limit=201", "/api/assets/asset/logs?limit=1&limit=2", "/api/assets/asset/logs?record_scope=%ZZ", "/api/assets/asset/logs?event_key=bad", "/api/assets/asset/logs?device_id=other", "/api/assets/asset/logs?contains=unbounded"} {
				before := repo.calls
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || before != repo.calls {
					t.Fatalf("invalid query reached Store: %s %d", path, w.Code)
				}
			}
		})
	}
}
