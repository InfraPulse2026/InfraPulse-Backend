package storage

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prometheus"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

func TestPrometheusTLSInventoryLatestRangeAtomicityAndPolicyFence(t *testing.T) {
	s := integrationStore(t)
	_, principal := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), principal)
	now := time.Now().UTC().Truncate(time.Second)
	var sourceID string
	var reviseOnQuery atomic.Bool
	var requestCount atomic.Int32
	var input datasources.Input
	server, ca := zabbixTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method != "GET" {
			t.Error("mutation")
		}
		var data any
		switch r.URL.Path {
		case "/prometheus/api/v1/status/buildinfo":
			data = map[string]string{"version": "3.15.0"}
		case "/prometheus/api/v1/targets":
			data = map[string]any{"activeTargets": []any{map[string]any{"scrapePool": "node", "labels": map[string]string{"job": "node", "instance": "host:9100"}, "lastScrape": now.Format(time.RFC3339Nano), "health": "up", "scrapeInterval": "15s"}}}
		case "/prometheus/api/v1/query":
			if !strings.Contains(r.URL.Query().Get("query"), `instance="host:9100"`) {
				t.Error("unscoped")
			}
			data = map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]string{}, "value": []any{now.Unix(), "40"}}}}
		case "/prometheus/api/v1/query_range":
			if reviseOnQuery.Swap(false) {
				input.Revision = 1
				input.Name = "changed while query ran"
				if _, err := s.SaveDataSource(ctx, sourceID, input); err != nil {
					t.Error(err)
				}
			}
			data = map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{}, "values": []any{[]any{now.Add(-time.Minute).Unix(), "30"}, []any{now.Unix(), "40"}}}}}
		default:
			t.Error("scraped targets/unapproved API", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	})
	policy, err := datasources.NewNetworkPolicy(datasources.NetworkConfig{AllowedOrigins: []string{server.URL}, AllowedCIDRs: []string{"127.0.0.0/8"}, AllowLoopback: true, CABundles: map[string][]byte{"fixture": ca}})
	if err != nil {
		t.Fatal(err)
	}
	connector := prometheus.New(policy)
	registry, err := s.CollectorOperations().With(connector.Operations()...)
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureDataSourceOperations(registry)
	s.ConfigurePrometheus(connector)
	input = datasources.Input{Type: "PROMETHEUS", Name: "Prometheus fixture", Endpoint: server.URL + "/prometheus", Enabled: true, SyncIntervalSeconds: 30, Configuration: datasources.Settings{TimeoutSeconds: 10, TLSCARef: "fixture", Prometheus: &prommodel.Settings{ScrapePools: []string{"node"}, DeviceType: "server", Mappings: []prommodel.Mapping{{ID: "cpu", Metric: "cpu.utilization", Unit: "percent", Expression: "avg(rate(cpu_seconds[5m]))*100"}}}}}
	source, err := s.SaveDataSource(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	sourceID = source.ID
	run := func(key string) datasources.Result {
		t.Helper()
		if _, _, err := s.QueueDataSourceJob(ctx, source.ID, "sync", key); err != nil {
			t.Fatal(err)
		}
		r := claimSourceJob(t, s, ctx)
		op, _ := registry.Lookup("PROMETHEUS", "sync")
		result, err := op.Run(ctx, *r)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishDataSourceJob(ctx, *r, result, nil); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := run("initial")
	items, err := s.Assets(ctx, "host", "real", 50, 0)
	if err != nil || len(items) != 1 {
		t.Fatal("target not published", items, err)
	}
	asset := items[0]
	target := first.Prometheus.Targets[0]
	metrics, err := s.ExternalMetrics(ctx, externalmonitor.Filter{AssetID: asset.ID, Limit: 50}, false)
	if err != nil || len(metrics) != 1 || metrics[0].Value == nil || *metrics[0].Value != "40" || metrics[0].SourceType != "PROMETHEUS" {
		t.Fatal("latest", metrics, err)
	}
	if _, err = s.ExternalMetrics(ctx, externalmonitor.Filter{SourceID: source.ID, Limit: 50}, true); err != prommodel.ErrHistory {
		t.Fatal("Prometheus history copied", err)
	}
	run("repeat")
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM prometheus_measurements`).Scan(&count); err != nil || count != 1 {
		t.Fatal("latest accumulated poll history", count, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM external_metric_samples WHERE source_id=$1`, source.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("Prometheus copied to Zabbix history", count, err)
	}
	q := prommodel.Query{AssetID: asset.ID, SourceID: source.ID, TargetID: target.ID, MappingID: "cpu", Kind: "range", From: now.Add(-time.Minute), To: now, StepSeconds: 60}
	before := requestCount.Load()
	wrong := q
	wrong.AssetID = "unrelated-asset"
	if _, err = s.PrometheusQuery(ctx, wrong); err != ErrNotFound || requestCount.Load() != before {
		t.Fatal("foreign Asset caused an upstream query", err)
	}
	chart, err := s.PrometheusQuery(ctx, q)
	if err != nil || len(chart.Points) != 2 || chart.SourceID != source.ID {
		t.Fatal("range", chart, err)
	}
	// Move source binding; graph queries must follow its current Asset ownership.
	bindings, _ := s.AssetBindings(ctx, asset.ID, true, 50, 0)
	if bindings[0].LegacyAgentID != nil || bindings[0].LegacyDeviceID != nil {
		t.Fatal("invented Agent")
	}
	_, err = s.Asset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Late source edits discard a query response, without writing any samples.
	reviseOnQuery.Store(true)
	if _, err = s.PrometheusQuery(ctx, q); err != datasources.ErrConflict {
		t.Fatal("stale query response returned after settings change", err)
	}
	metrics, err = s.ExternalMetrics(ctx, externalmonitor.Filter{AssetID: asset.ID, Limit: 50}, false)
	if err != nil || metrics[0].Quality != "configuration_changed" {
		t.Fatal("old mapping shown current", metrics, err)
	}
}
