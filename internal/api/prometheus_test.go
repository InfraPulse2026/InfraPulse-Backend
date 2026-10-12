package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type promStub struct {
	assetStub
	queries int
	last    prommodel.Query
}

func (s *promStub) PrometheusQuery(_ context.Context, q prommodel.Query) (prommodel.QueryResult, error) {
	s.queries++
	s.last = q
	return prommodel.QueryResult{SourceID: q.SourceID, AssetID: q.AssetID, TargetID: q.TargetID, MappingID: q.MappingID, Points: []prommodel.Point{}, Metadata: []prommodel.Metadata{}}, nil
}
func TestPrometheusChartRolesBoundsAndNoRawQueries(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &promStub{}
			handler := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})), WithAssets(repo))
			now := time.Now().UTC()
			params := url.Values{"source_instance_id": {"src_prom"}, "target_id": {prommodel.TargetID("node", map[string]string{"job": "node", "instance": "host"})}, "mapping_id": {"cpu"}, "from": {now.Add(-time.Hour).Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}}
			for _, kind := range []string{"instant", "range", "metadata"} {
				path := "/api/assets/a/prometheus/" + kind + "?" + params.Encode()
				request := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 401 {
					t.Fatal("unprotected Prometheus read", w.Code)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 200 || repo.last.Kind != kind || repo.last.AssetID != "a" {
					t.Fatal("role/read contract", role, w.Code, repo.last)
				}
			}
			before := repo.queries
			for _, extra := range []string{"&query=up", "&endpoint=https://other.example", "&step_seconds=1", "&source_instance_id=another"} {
				request := httptest.NewRequest("GET", "/api/assets/a/prometheus/range?"+params.Encode()+extra, nil)
				request.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 400 || repo.queries != before {
					t.Fatal("query substitution/bounds reached upstream", extra, w.Code)
				}
			}
		})
	}
}
