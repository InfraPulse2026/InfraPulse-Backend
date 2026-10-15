package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
)

type assetStub struct {
	calls       int
	kind, scope string
	inventory   bool
}

func (s *assetStub) Assets(_ context.Context, kind, scope string, _ int, _ int) ([]assets.Asset, error) {
	s.calls++
	s.kind = kind
	s.scope = scope
	return []assets.Asset{}, nil
}
func (s *assetStub) Asset(context.Context, string) (assets.Asset, error) {
	s.calls++
	return assets.Asset{ID: "fixture", ResolutionStatus: "unresolved"}, nil
}
func (s *assetStub) AssetBindings(_ context.Context, _ string, inventory bool, _ int, _ int) ([]assets.Binding, error) {
	s.calls++
	s.inventory = inventory
	return []assets.Binding{}, nil
}
func (s *assetStub) AssetFacts(context.Context, string, int, int) ([]assets.FactVersion, error) {
	s.calls++
	return []assets.FactVersion{}, nil
}
func (s *assetStub) AssetIdentities(context.Context, string, int, int) ([]assets.Identity, error) {
	s.calls++
	return []assets.Identity{}, nil
}

func TestAssetRoutesReadPermissionsFiltersAndInventory(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &assetStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})), WithAssets(repo))
			for _, path := range []string{"/api/assets", "/api/assets/fixture", "/api/assets/fixture/sources", "/api/assets/fixture/inventory", "/api/assets/fixture/facts", "/api/assets/fixture/identities"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", path, nil)
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("unprotected asset route", path, w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("read denied", path, w.Code)
				}
				if path == "/api/assets/fixture/inventory" && !repo.inventory {
					t.Fatal("inventory omitted")
				}
				if path == "/api/assets/fixture/sources" && repo.inventory {
					t.Fatal("sources unexpectedly includes snapshots")
				}
			}
			before := repo.calls
			for _, query := range []string{"kind=unknown-kind", "scope=production", "limit=201", "offset=-1"} {
				r := httptest.NewRequest("GET", "/api/assets?"+query, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.calls != before {
					t.Fatal("invalid asset filter reached storage", query, w.Code)
				}
			}
			r := httptest.NewRequest("GET", "/api/assets?kind=system&scope=real", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || repo.kind != "system" || repo.scope != "real" {
				t.Fatal("asset filters changed")
			}
		})
	}
}
