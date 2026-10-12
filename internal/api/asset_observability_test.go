package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetvoip"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type observabilityStub struct {
	assetStub
	hardwareCalls, voipReads, voipWrites int
	history                              bool
	failure                              error
}

func (s *observabilityStub) AssetHardware(context.Context, string, assethardware.Filter) (assethardware.Page, error) {
	s.hardwareCalls++
	return assethardware.Page{Items: []assethardware.Item{}}, s.failure
}
func (s *observabilityStub) AssetHardwareHistory(context.Context, string, assethardware.HistoryFilter) (assethardware.HistoryPage, error) {
	s.hardwareCalls++
	s.history = true
	return assethardware.HistoryPage{Items: []assethardware.Item{}}, s.failure
}
func (s *observabilityStub) AssetVoIP(context.Context, string, int, int) (assetvoip.Card, error) {
	s.voipReads++
	return assetvoip.Card{Items: []assetvoip.Service{}}, s.failure
}
func (s *observabilityStub) SaveAssetVoIP(context.Context, string, string, assetvoip.Input, string) (assetvoip.Association, bool, error) {
	s.voipWrites++
	return assetvoip.Association{ID: "association"}, true, s.failure
}
func (s *observabilityStub) AssetVoIPHistory(context.Context, string, string, assetvoip.HistoryFilter) (assetvoip.HistoryPage, error) {
	s.voipReads++
	return assetvoip.HistoryPage{}, s.failure
}
func (s *observabilityStub) AssetVoIPEvents(context.Context, string, string, int, int) ([]assetvoip.Event, error) {
	s.voipReads++
	return []assetvoip.Event{}, s.failure
}
func TestAssetHardwareVoIPRolesBoundsAndConfirmedWrites(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &observabilityStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAssets(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/assets/asset/hardware", "/api/assets/asset/hardware/history", "/api/assets/asset/voip", "/api/assets/asset/voip/association/history", "/api/assets/asset/voip/association/events"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("unprotected", path, w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("read denied", path, w.Code, w.Body.String())
				}
			}
			beforeHW, beforeVoIP := repo.hardwareCalls, repo.voipReads
			for _, path := range []string{"/api/assets/asset/hardware?include_related=maybe", "/api/assets/asset/hardware?max_age_seconds=86401", "/api/assets/asset/hardware?limit=201", "/api/assets/asset/hardware?limit=%zz", "/api/assets/asset/hardware?include_related=true&include_related=false", "/api/assets/asset/hardware/history?after_id=-1", "/api/assets/asset/voip?offset=-1", "/api/assets/asset/voip/association/history?from=tomorrow", "/api/assets/asset/voip/association/history?before_id=9223372036854775808"} {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.hardwareCalls != beforeHW || repo.voipReads != beforeVoIP {
					t.Fatal("badquery reachedstore", path, w.Code)
				}
			}
			body := `{"binding_id":"binding","agent_id":"agent","target_id":"sip","address":"pbx.example:5061","transport":"tls","source":"sip.options","scope":"","service_kind":"pbx","name":"PBX","expected_asset_revision":1,"expected_revision":0,"enabled":true,"confirmed":true,"reason":"reviewed source identity"}`
			r := httptest.NewRequest("POST", "/api/assets/asset/voip", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "reviewed-voip")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			expected := 403
			if role == "admin" {
				expected = 200
			}
			if w.Code != expected {
				t.Fatal("VoIP permissions", w.Code, w.Body.String())
			}
			if role == "admin" && w.Header().Get("Idempotent-Replay") != "true" {
				t.Fatal("replay missing")
			}
			if role != "admin" && repo.voipWrites != 0 {
				t.Fatal("forbidden writes reachedstore")
			}
			if role == "admin" {
				before := repo.voipWrites
				for _, bad := range []string{strings.Replace(body, `"confirmed":true`, `"confirmed":false`, 1), strings.Replace(body, `"expected_revision":0`, `"expected_revision":-1`, 1), strings.Replace(body, `"reason":"reviewed source identity"`, `"reason":"reviewed source identity","raw":true`, 1)} {
					r := httptest.NewRequest("POST", "/api/assets/asset/voip", strings.NewReader(bad))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "bad-voip")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 400 || repo.voipWrites != before {
						t.Fatal("invalid mutation reachedstore", w.Code)
					}
				}
			}
		})
	}
}
