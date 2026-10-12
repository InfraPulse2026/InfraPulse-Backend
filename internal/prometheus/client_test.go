package prometheus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type fakeHTTP struct {
	t     *testing.T
	calls []string
	reply func(datasources.HTTPRequest) datasources.HTTPResponse
}

func (f *fakeHTTP) Close() {}
func (f *fakeHTTP) Do(_ context.Context, r datasources.HTTPRequest) (datasources.HTTPResponse, error) {
	if r.Method != "GET" {
		f.t.Fatal("mutation method")
	}
	f.calls = append(f.calls, r.Path)
	return f.reply(r), nil
}
func response(data any) datasources.HTTPResponse {
	raw, _ := json.Marshal(map[string]any{"status": "success", "data": data})
	return datasources.HTTPResponse{StatusCode: 200, Body: raw}
}
func fixture(t *testing.T) (*Connector, *fakeHTTP, datasources.Request) {
	t.Helper()
	f := &fakeHTTP{t: t}
	c := &Connector{open: func(datasources.Source) (transport, error) { return f, nil }, slots: make(chan struct{}, 4)}
	cfg, _ := (prommodel.Settings{ScrapePools: []string{"node"}, DeviceType: "server", AuthMode: "bearer", Mappings: []prommodel.Mapping{{ID: "cpu", Metric: "cpu.utilization", Unit: "percent", Expression: `avg(rate(cpu_seconds[5m]))*100`, MetadataMetric: "cpu_seconds"}}}).Normalize()
	r := datasources.Request{Source: datasources.Source{ID: "src_prom", Type: "PROMETHEUS", Revision: 1, Enabled: true, Configuration: datasources.Settings{TimeoutSeconds: 15, Prometheus: &cfg}}, Credential: []byte("fixture-prom-token")}
	return c, f, r
}
func TestPrometheusDiscoveryInstantRangeMetadataAndErrors(t *testing.T) {
	c, f, r := fixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.reply = func(req datasources.HTTPRequest) datasources.HTTPResponse {
		if string(req.Bearer) != "fixture-prom-token" {
			t.Fatal("bearer missing")
		}
		switch req.Path {
		case "api/v1/status/buildinfo":
			return response(map[string]string{"version": "3.15.0"})
		case "api/v1/targets":
			if req.Query.Get("state") != "active" || req.Query.Get("scrapePool") != "node" {
				t.Fatal("unbounded target discovery")
			}
			return response(map[string]any{"activeTargets": []any{map[string]any{"scrapePool": "node", "labels": map[string]string{"job": "node", "instance": "host:9100", "token": "do-not-return"}, "health": "up", "lastScrape": now.Format(time.RFC3339Nano), "scrapeInterval": "15s", "scrapeUrl": "http://user:password@169.254.169.254/metrics", "lastError": "fixture-prom-token"}}})
		case "api/v1/query":
			if !strings.Contains(req.Query.Get("query"), `instance="host:9100"`) || !strings.Contains(req.Query.Get("query"), `job="node"`) {
				t.Fatal("unscoped query", req.Query.Get("query"))
			}
			return response(map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]string{}, "value": []any{now.Unix(), "25.125"}}}})
		case "api/v1/query_range":
			return response(map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{}, "values": []any{[]any{now.Add(-time.Minute).Unix(), "25"}, []any{now.Unix(), "NaN"}}}}})
		case "api/v1/targets/metadata":
			if strings.Contains(req.Query.Get("match_target"), "__name__") || req.Query.Get("metric") != "cpu_seconds" {
				t.Fatal("bad metadata selector")
			}
			return response([]any{map[string]any{"target": map[string]string{"job": "node", "instance": "host:9100"}, "metric": "cpu_seconds", "type": "counter", "unit": "seconds", "help": "CPU password=hidden"}})
		default:
			t.Fatal("target scraping or unapproved path", req.Path)
			return response(nil)
		}
	}
	result, err := c.sync(t.Context(), r)
	if err != nil || len(result.Observations) != 1 || len(result.Prometheus.Measurements) != 1 {
		t.Fatal(result, err)
	}
	raw, _ := json.Marshal(result.Prometheus)
	if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "169.254") || strings.Contains(string(raw), "do-not-return") {
		t.Fatal("unapproved discovery data leaked")
	}
	if result.Observations[0].Device.Online || result.Observations[0].Device.Hostname != "" || len(result.Observations[0].Device.IdentityEvidence) != 0 {
		t.Fatal("instance invented physical identity")
	}
	target := result.Prometheus.Targets[0]
	v := result.Prometheus.Measurements[0]
	if v.Value == nil || *v.Value != "25.125" || v.ScrapedAt == nil || v.TimeSemantics != "promql_evaluation" {
		t.Fatal(v)
	}
	q := prommodel.Query{AssetID: "asset", SourceID: r.Source.ID, TargetID: target.ID, MappingID: "cpu", Kind: "range", From: now.Add(-time.Minute), To: now, StepSeconds: 60}
	chart, err := c.Query(t.Context(), r.Source, r.Credential, target, q)
	if err != nil || len(chart.Points) != 2 || chart.Points[1].Value != nil || chart.Points[1].Quality != "non_finite" || chart.Quality != "partial" {
		t.Fatal(chart, err)
	}
	q.Kind = "metadata"
	meta, err := c.Query(t.Context(), r.Source, r.Credential, target, q)
	if err != nil || len(meta.Metadata) != 1 || strings.Contains(meta.Metadata[0].Help, "hidden") {
		t.Fatal(meta, err)
	}
	f.reply = func(req datasources.HTTPRequest) datasources.HTTPResponse {
		if req.Path == "api/v1/status/buildinfo" {
			return response(map[string]string{"version": "3.15.0"})
		}
		return datasources.HTTPResponse{StatusCode: 200, Body: []byte(`{"status":"success","data":{},"warnings":["fixture-prom-token"]}`)}
	}
	result, err = c.sync(t.Context(), r)
	code, _ := datasources.Outcome(err)
	if code != "partial_response" || result.Prometheus != nil || len(result.Cursor) != 0 || strings.Contains(err.Error(), "fixture") {
		t.Fatal("partial success or error leak", code)
	}
}
func TestStaleScrapeDoesNotBecomeFreshAndAmbiguity(t *testing.T) {
	_, f, r := fixture(t)
	now := time.Now()
	scraped := now.Add(-time.Hour)
	target := prommodel.Target{ID: "target", Identity: map[string]string{"job": "node", "instance": "host"}, LastScrape: &scraped, Health: "up"}
	f.reply = func(req datasources.HTTPRequest) datasources.HTTPResponse {
		return response(map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]string{}, "value": []any{now.Unix(), "20"}}}})
	}
	p := &client{http: f, source: r.Source, cfg: *r.Source.Configuration.Prometheus, credential: r.Credential}
	v, err := p.instant(t.Context(), target, p.cfg.Mappings[0], now)
	if err != nil || v.Quality != "stale" {
		t.Fatal("fresh eval hid stale scrape", v, err)
	}
	f.reply = func(req datasources.HTTPRequest) datasources.HTTPResponse {
		return response(map[string]any{"resultType": "vector", "result": []any{map[string]any{}, map[string]any{}}})
	}
	v, err = p.instant(t.Context(), target, p.cfg.Mappings[0], now)
	if err != nil || v.Quality != "ambiguous" || v.Value != nil {
		t.Fatal("multiple series guessed a value", v, err)
	}
}
