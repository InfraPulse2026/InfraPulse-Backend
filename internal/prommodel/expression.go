package prommodel

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"sort"
	"time"
)

// ScopedExpression narrows EVERY vector selector through the official AST.
// It cannot broaden configured matchers or be escaped by target label values.
func ScopedExpression(expression string, identity map[string]string) (string, error) {
	if !Text(expression, 4096) || identity["job"] == "" || identity["instance"] == "" {
		return "", ErrInvalid
	}
	ast, err := parser.NewParser(parser.Options{}).ParseExpr(expression)
	if err != nil {
		return "", ErrInvalid
	}
	keys := []string{}
	for k, v := range identity {
		if !LabelName(k) || v == "" || !Text(v, 512) {
			return "", ErrInvalid
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	count, vectors := 0, 0
	bad := false
	parser.Inspect(ast, func(node parser.Node, path []parser.Node) error {
		if node == nil {
			return nil
		}
		count++
		if count > 200 || len(path) > 24 {
			bad = true
			return ErrInvalid
		}
		switch v := node.(type) {
		case *parser.VectorSelector:
			if v.Timestamp != nil || v.StartOrEnd != 0 || v.OriginalOffset < 0 || v.OriginalOffset > 30*24*time.Hour {
				bad = true
				return ErrInvalid
			}
			vectors++
			for _, k := range keys {
				matcher, err := labels.NewMatcher(labels.MatchEqual, k, identity[k])
				if err != nil {
					bad = true
					return ErrInvalid
				}
				v.LabelMatchers = append(v.LabelMatchers, matcher)
			}
		case *parser.MatrixSelector:
			if v.Range > 30*24*time.Hour {
				bad = true
				return ErrInvalid
			}
		case *parser.SubqueryExpr:
			if v.Range > 30*24*time.Hour || v.OriginalOffset < 0 || v.OriginalOffset > 30*24*time.Hour || v.Timestamp != nil || v.StartOrEnd != 0 || v.Step > 0 && v.Step < 3*time.Second {
				bad = true
				return ErrInvalid
			}
		}
		return nil
	})
	if bad || vectors == 0 {
		return "", ErrInvalid
	}
	return ast.String(), nil
}

func MetadataSelector(scoped string) (string, error) {
	ast, err := parser.NewParser(parser.Options{}).ParseExpr(scoped)
	if err != nil {
		return "", ErrInvalid
	}
	v, ok := ast.(*parser.VectorSelector)
	if !ok {
		return "", ErrInvalid
	}
	v.Name = ""
	filtered := []*labels.Matcher{}
	for _, m := range v.LabelMatchers {
		if m.Name != "__name__" {
			filtered = append(filtered, m)
		}
	}
	v.LabelMatchers = filtered
	return v.String(), nil
}

func NodeExporterMappings() []Mapping {
	return []Mapping{
		{ID: "up", Metric: "availability", Unit: "boolean", Expression: "up", MetadataMetric: "up"},
		{ID: "cpu", Metric: "cpu.utilization", Unit: "percent", Expression: `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`, MetadataMetric: "node_cpu_seconds_total"},
		{ID: "ram", Metric: "memory.utilization", Unit: "percent", Expression: `100 * (1 - sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes))`, MetadataMetric: "node_memory_MemAvailable_bytes"},
		{ID: "disk", Metric: "disk.utilization", Unit: "percent", Expression: `100 * (1 - sum(node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|squashfs|proc|sysfs"}) / sum(node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs|proc|sysfs"}))`, MetadataMetric: "node_filesystem_avail_bytes"},
		{ID: "rx", Metric: "network.rx", Unit: "bytes_per_second", Expression: `sum(rate(node_network_receive_bytes_total{device!="lo"}[5m]))`, MetadataMetric: "node_network_receive_bytes_total"},
		{ID: "tx", Metric: "network.tx", Unit: "bytes_per_second", Expression: `sum(rate(node_network_transmit_bytes_total{device!="lo"}[5m]))`, MetadataMetric: "node_network_transmit_bytes_total"},
		{ID: "errors", Metric: "network.errors", Unit: "per_second", Expression: `sum(rate(node_network_receive_errs_total{device!="lo"}[5m])) + sum(rate(node_network_transmit_errs_total{device!="lo"}[5m]))`, MetadataMetric: "node_network_receive_errs_total"},
	}
}
