package prommodel

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"strings"
	"testing"
	"time"
)

func TestExpressionNarrowsAllSelectorsWithoutLabelInjection(t *testing.T) {
	identity := map[string]string{"job": "node", "instance": "quoted\"host:9100", "cluster": "a"}
	expr, err := ScopedExpression(`sum(rate(node_network_receive_bytes_total[5m])) + sum(rate(node_network_transmit_bytes_total[5m]))`, identity)
	if err != nil {
		t.Fatal(err)
	}
	ast, err := parser.NewParser(parser.Options{}).ParseExpr(expr)
	if err != nil {
		t.Fatal("escaped expression does not parse", err)
	}
	count := 0
	parser.Inspect(ast, func(n parser.Node, _ []parser.Node) error {
		if v, ok := n.(*parser.VectorSelector); ok {
			count++
			for key, value := range identity {
				found := false
				for _, m := range v.LabelMatchers {
					if m.Name == key && m.Type == labels.MatchEqual && m.Value == value {
						found = true
					}
				}
				if !found {
					t.Fatal("unscoped selector", key)
				}
			}
		}
		return nil
	})
	if count != 2 {
		t.Fatal("selector count", count)
	}
	for _, unsafe := range []string{"vector(1)", "1+2", "up @ 123", "up offset -1h", "rate(up[40d])", "up @ end()", "rate(up[1h:1ms])"} {
		if _, err = ScopedExpression(unsafe, map[string]string{"job": "node", "instance": "host"}); err == nil {
			t.Fatal("unsafe/unsupported expression", unsafe)
		}
	}
	s, err := (Settings{ScrapePools: []string{"node"}, Profile: "node_exporter"}).Normalize()
	if err != nil || len(s.Mappings) != 7 || s.DeviceType != "server" || s.PageSize != 5 {
		t.Fatal("profile", s, err)
	}
	for _, m := range s.Mappings {
		if _, err = ScopedExpression(m.Expression, identity); err != nil {
			t.Fatal("profile expression", m.ID, err)
		}
	}
	metadata, err := MetadataSelector(`node_cpu_seconds_total{job="node",instance="host"}`)
	if err != nil || strings.Contains(metadata, "__name__") || strings.Contains(metadata, "node_cpu_seconds_total") {
		t.Fatal("metadata matched a metric label on targets", metadata, err)
	}
}
func TestTargetIdentityAndQueryBounds(t *testing.T) {
	a := TargetID("node", map[string]string{"job": "node", "instance": "host:9100", "cluster": "a"})
	b := TargetID("node", map[string]string{"job": "node", "instance": "host:9100", "cluster": "b"})
	if a == b || !TargetIDValid(a) || !MetricIDValid(MetricID(a, "cpu")) {
		t.Fatal("target/mapping identity")
	}
	for _, s := range []Settings{{IdentityLabels: []string{"instance"}}, {IdentityLabels: []string{"job", "instance", "token"}}, {PageSize: 6}, {MaxTargets: 201}, {AuthMode: "basic"}, {LabelMatch: map[string]string{"secret": "value"}}} {
		if _, err := s.Normalize(); err == nil {
			t.Fatal("unsafe settings accepted", s)
		}
	}
	now := time.Now()
	q := Query{AssetID: "asset", SourceID: "source", TargetID: a, MappingID: "cpu", Kind: "range", From: now.Add(-time.Hour), To: now, StepSeconds: 60}
	if q.Validate(now) != nil {
		t.Fatal("valid chart rejected")
	}
	q.StepSeconds = 1
	if q.Validate(now) == nil {
		t.Fatal("small step accepted")
	}
	q.StepSeconds = 3
	q.From = now.Add(-14 * 24 * time.Hour)
	if q.Validate(now) == nil {
		t.Fatal("too many points accepted")
	}
}
