package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
)

type globalTopologyStub struct {
	assetStub
	calls  int
	filter topology.GlobalFilter
	after  int64
}

func (s *globalTopologyStub) GlobalTopology(_ context.Context, f topology.GlobalFilter) (topology.GlobalGraph, error) {
	s.calls++
	s.filter = f
	return topology.GlobalGraph{Nodes: []topology.GlobalNode{}, Edges: []topology.GlobalEdge{}, ManagementRelations: []topology.ManagementRelation{}}, nil
}
func (s *globalTopologyStub) GlobalTopologyChanges(_ context.Context, source, scope, asset string, after int64, limit int) (storage.TopologyChangePage, error) {
	s.calls++
	s.filter = topology.GlobalFilter{SourceID: source, Scope: scope, AssetID: asset, Limit: limit}
	s.after = after
	return storage.TopologyChangePage{Items: []storage.TopologyChange{}}, nil
}
func TestGlobalTopologyAPIPermissionsAndStrictQueries(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "reader", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &globalTopologyStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAssets(repo), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})))
			for _, path := range []string{"/api/topology/global", "/api/topology?view=global", "/api/assets/asset/neighbors", "/api/topology/changes?after_id=12"} {
				r := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("unprotected route", path, w.Code)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("read denied", path, w.Code, w.Body.String())
				}
				if path == "/api/assets/asset/neighbors" && repo.filter.AssetID != "asset" {
					t.Fatal("wrong asset path")
				}
			}
			if repo.after != 12 {
				t.Fatal("history cursor dropped")
			}
			before := repo.calls
			for _, path := range []string{"/api/topology/global?scope=prod", "/api/topology/global?limit=2001", "/api/topology/global?include_expired=maybe", "/api/topology/global?source_instance_id=a&source_instance_id=b", "/api/topology/changes?after_id=-1", "/api/topology/changes?after_id=9223372036854775808", "/api/assets/asset/neighbors?asset_id=other", "/api/topology/global?agent_id=another-collector", "/api/topology?view=global&view=legacy"} {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.calls != before {
					t.Fatal("invalid query reached repo", path, w.Code)
				}
			}
			r := httptest.NewRequest("GET", "/api/topology", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || repo.calls != before {
				t.Fatal("legacy default changed", w.Code)
			}
		})
	}
}
