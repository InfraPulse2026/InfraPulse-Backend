package prometheus

import (
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"strings"
	"testing"
	"time"
)

func TestPrometheusAlertsScopedDedupSanitizedAndPending(t *testing.T) {
	_, http, r := fixture(t)
	cfg := *r.Source.Configuration.Prometheus
	cfg.AlertsEnabled = true
	p := &client{http: http, cfg: cfg, source: r.Source, credential: r.Credential}
	now := time.Now().UTC()
	target := prommodel.Target{ID: prommodel.TargetID("node", map[string]string{"job": "node", "instance": "host:9100"}), Identity: map[string]string{"job": "node", "instance": "host:9100"}}
	alert := func(instance, state string) map[string]any {
		return map[string]any{"state": state, "activeAt": now.Format(time.RFC3339Nano), "labels": map[string]string{"job": "node", "instance": instance, "alertname": "CPU high", "severity": "critical", "token": "private-value"}, "annotations": map[string]string{"summary": "password=hidden"}}
	}
	http.reply = func(req datasources.HTTPRequest) datasources.HTTPResponse {
		if req.Path != "api/v1/alerts" {
			t.Fatal(req.Path)
		}
		return response(map[string]any{"alerts": []any{alert("host:9100", "firing"), alert("host:9100", "firing"), alert("other:9100", "firing"), alert("host:9100", "pending")}})
	}
	alerts, err := p.alerts(t.Context(), []prommodel.Target{target}, []prommodel.Target{target}, now)
	if err != nil || len(alerts) != 1 || alerts[0].Severity != "critical" {
		t.Fatalf("alerts=%+v %v", alerts, err)
	}
	raw, _ := json.Marshal(alerts)
	if strings.Contains(string(raw), "private-value") || strings.Contains(string(raw), "password") {
		t.Fatal("raw labels/annotations leaked")
	}
	other := target
	other.ID = prommodel.TargetID("another-pool", target.Identity)
	alerts, err = p.alerts(t.Context(), []prommodel.Target{target, other}, []prommodel.Target{target}, now)
	if err != nil || len(alerts) != 0 {
		t.Fatal("ambiguous target guessed", alerts, err)
	}
}
func TestPrometheusAlertsMalformedAndPartialReject(t *testing.T) {
	_, http, r := fixture(t)
	p := &client{http: http, cfg: *r.Source.Configuration.Prometheus, source: r.Source}
	now := time.Now().UTC()
	target := prommodel.Target{ID: "target", Identity: map[string]string{"job": "node", "instance": "host"}}
	http.reply = func(datasources.HTTPRequest) datasources.HTTPResponse {
		return response(map[string]any{"alerts": []any{map[string]any{"state": "firing", "activeAt": "broken", "labels": map[string]string{"job": "node", "instance": "host", "alertname": "Bad"}}}})
	}
	if _, err := p.alerts(t.Context(), []prommodel.Target{target}, []prommodel.Target{target}, now); err == nil {
		t.Fatal("bad timestamp accepted")
	}
	http.reply = func(datasources.HTTPRequest) datasources.HTTPResponse {
		return datasources.HTTPResponse{StatusCode: 200, Body: []byte(`{"status":"success","data":{"alerts":[]},"warnings":["partial"]}`)}
	}
	if _, err := p.alerts(t.Context(), nil, nil, now); err == nil {
		t.Fatal("partial response accepted")
	}
}
