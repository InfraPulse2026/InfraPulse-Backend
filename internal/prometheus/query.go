package prometheus

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"strconv"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type queryData struct {
	Type   string          `json:"resultType"`
	Result json.RawMessage `json:"result"`
}
type series struct {
	Labels     map[string]string   `json:"metric"`
	Value      []json.RawMessage   `json:"value"`
	Values     [][]json.RawMessage `json:"values"`
	Histogram  json.RawMessage     `json:"histogram"`
	Histograms json.RawMessage     `json:"histograms"`
}

func timestamp(raw json.RawMessage) (time.Time, error) {
	v, ok := externalmonitor.Number(string(raw))
	if !ok || v.Sign() < 0 {
		return time.Time{}, datasources.Failure{Code: "invalid_response"}
	}
	seconds := new(big.Int).Quo(v.Num(), v.Denom())
	if !seconds.IsInt64() || seconds.Int64() > 253402300799 {
		return time.Time{}, datasources.Failure{Code: "invalid_response"}
	}
	part := new(big.Rat).Sub(v, new(big.Rat).SetInt(seconds))
	part.Mul(part, big.NewRat(1000000000, 1))
	ns := new(big.Int).Quo(part.Num(), part.Denom())
	return time.Unix(seconds.Int64(), ns.Int64()).UTC(), nil
}
func point(raw []json.RawMessage, m prommodel.Mapping) (prommodel.Point, error) {
	if len(raw) != 2 {
		return prommodel.Point{}, datasources.Failure{Code: "invalid_response"}
	}
	at, err := timestamp(raw[0])
	if err != nil {
		return prommodel.Point{}, err
	}
	var value string
	if json.Unmarshal(raw[1], &value) != nil {
		return prommodel.Point{}, datasources.Failure{Code: "invalid_response"}
	}
	p := prommodel.Point{At: at, Quality: "valid"}
	if value == "NaN" || value == "+Inf" || value == "-Inf" {
		p.Quality = "non_finite"
		return p, nil
	}
	normal, ok := externalmonitor.NormalizeValue(value, externalmonitor.Mapping{Multiplier: m.Multiplier, Unit: m.Unit})
	if !ok || len(normal) > 256 {
		p.Quality = "invalid_value"
		return p, nil
	}
	p.Value = &normal
	return p, nil
}
func scopedLabels(actual, expected map[string]string) bool {
	for k, v := range expected {
		if got, ok := actual[k]; ok && got != v {
			return false
		}
	}
	return true
}
func (c *client) evaluate(ctx context.Context, t prommodel.Target, m prommodel.Mapping, kind string, from, to time.Time, step int) ([]prommodel.Point, string, error) {
	expression, err := prommodel.ScopedExpression(m.Expression, t.Identity)
	if err != nil {
		return nil, "", datasources.Failure{Code: "configuration_required"}
	}
	params := url.Values{"query": {expression}, "timeout": {strconv.Itoa(min(c.source.Configuration.TimeoutSeconds, 10)) + "s"}, "limit": {"2"}}
	path := "api/v1/query"
	if kind == "range" {
		path = "api/v1/query_range"
		params.Set("start", from.Format(time.RFC3339Nano))
		params.Set("end", to.Format(time.RFC3339Nano))
		params.Set("step", strconv.Itoa(step)+"s")
	} else {
		params.Set("time", to.Format(time.RFC3339Nano))
	}
	var data queryData
	if err = c.get(ctx, path, params, &data); err != nil {
		return nil, "", err
	}
	points := []prommodel.Point{}
	if data.Type == "scalar" && kind == "instant" {
		var value []json.RawMessage
		if json.Unmarshal(data.Result, &value) != nil {
			return nil, "", datasources.Failure{Code: "invalid_response"}
		}
		p, err := point(value, m)
		if err != nil {
			return nil, "", err
		}
		points = append(points, p)
	} else {
		if data.Type != "vector" && data.Type != "matrix" {
			return points, "unsupported", nil
		}
		if kind == "instant" && data.Type != "vector" {
			return points, "unsupported", nil
		}
		if kind == "range" && data.Type != "matrix" {
			return nil, "", datasources.Failure{Code: "invalid_response"}
		}
		var values []series
		if json.Unmarshal(data.Result, &values) != nil {
			return nil, "", datasources.Failure{Code: "invalid_response"}
		}
		if len(values) == 0 {
			return points, "no_data", nil
		}
		if len(values) > 1 {
			return points, "ambiguous", nil
		}
		v := values[0]
		if !scopedLabels(v.Labels, t.Identity) {
			return nil, "", datasources.Failure{Code: "invalid_response"}
		}
		if len(v.Histogram) > 0 || len(v.Histograms) > 0 {
			return points, "unsupported", nil
		}
		if data.Type == "vector" {
			p, err := point(v.Value, m)
			if err != nil {
				return nil, "", err
			}
			points = append(points, p)
		} else {
			if len(v.Values) > 3000 {
				return nil, "", datasources.Failure{Code: "response_too_large"}
			}
			for _, raw := range v.Values {
				p, err := point(raw, m)
				if err != nil {
					return nil, "", err
				}
				if p.At.Before(from) || p.At.After(to) || len(points) > 0 && !p.At.After(points[len(points)-1].At) {
					return nil, "", datasources.Failure{Code: "invalid_response"}
				}
				points = append(points, p)
			}
		}
	}
	quality := "complete"
	for _, p := range points {
		if p.At.After(to.Add(time.Second)) {
			return nil, "", datasources.Failure{Code: "invalid_response"}
		}
		if p.Quality != "valid" {
			quality = "partial"
		}
	}
	if len(points) == 0 {
		quality = "no_data"
	}
	return points, quality, nil
}
func (c *client) instant(ctx context.Context, t prommodel.Target, m prommodel.Mapping, now time.Time) (externalmonitor.Measurement, error) {
	points, quality, err := c.evaluate(ctx, t, m, "instant", time.Time{}, now, 0)
	if err != nil {
		return externalmonitor.Measurement{}, err
	}
	v := externalmonitor.Measurement{HostID: t.ID, ItemID: prommodel.MetricID(t.ID, m.ID), MappingID: m.ID, Name: m.ID, Metric: m.Metric, Unit: m.Unit, SourceUnit: m.Unit, Multiplier: m.Multiplier, MaxAgeSeconds: m.MaxAgeSeconds, Quality: quality, TimeSemantics: "promql_evaluation", ScrapedAt: t.LastScrape}
	if len(points) == 1 {
		p := points[0]
		v.ObservedAt = &p.At
		v.Nanoseconds = p.At.Nanosecond()
		v.Value = p.Value
		v.Quality = p.Quality
	}
	if v.Quality == "valid" {
		v.Quality = "fresh"
	}
	// Evaluated output time is not the scrape time of the underlying series.
	if v.Quality == "fresh" {
		if t.LastScrape == nil {
			v.Quality = "no_data"
		} else if t.LastScrape.After(now.Add(30 * time.Second)) {
			v.Quality = "clock_skew"
		} else if now.Sub(*t.LastScrape) > time.Duration(m.MaxAgeSeconds)*time.Second {
			v.Quality = "stale"
		} else if t.Health != "up" && m.Metric != "availability" {
			v.Quality = "source_unavailable"
		}
	}
	return v, nil
}
func (c *client) metadata(ctx context.Context, t prommodel.Target, m prommodel.Mapping) ([]prommodel.Metadata, error) {
	if m.MetadataMetric == "" {
		return []prommodel.Metadata{}, nil
	}
	selector, err := prommodel.ScopedExpression(m.MetadataMetric, t.Identity)
	if err != nil {
		return nil, datasources.Failure{Code: "configuration_required"}
	}
	matcher, err := prommodel.MetadataSelector(selector)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Target map[string]string `json:"target"`
		Metric string            `json:"metric"`
		Type   string            `json:"type"`
		Help   string            `json:"help"`
		Unit   string            `json:"unit"`
	}
	if err = c.get(ctx, "api/v1/targets/metadata", url.Values{"match_target": {matcher}, "metric": {m.MetadataMetric}, "limit": {"2"}}, &raw); err != nil {
		return nil, err
	}
	if len(raw) > 2 {
		return nil, datasources.Failure{Code: "response_too_large"}
	}
	items := []prommodel.Metadata{}
	for _, v := range raw {
		if v.Metric != m.MetadataMetric || !scopedLabels(v.Target, t.Identity) {
			return nil, datasources.Failure{Code: "invalid_response"}
		}
		help, err := c.clean(v.Help, 2048)
		if err != nil {
			return nil, err
		}
		unit, err := c.clean(v.Unit, 32)
		if err != nil {
			return nil, err
		}
		kind, err := c.clean(v.Type, 32)
		if err != nil {
			return nil, err
		}
		items = append(items, prommodel.Metadata{Metric: v.Metric, Type: kind, Unit: unit, Help: help})
	}
	return items, nil
}
func (c *Connector) Query(ctx context.Context, source datasources.Source, credential []byte, t prommodel.Target, q prommodel.Query) (prommodel.QueryResult, error) {
	now := time.Now().UTC()
	if err := q.Validate(now); err != nil {
		return prommodel.QueryResult{}, err
	}
	if err := c.acquire(ctx); err != nil {
		return prommodel.QueryResult{}, err
	}
	defer c.release()
	p, err := c.connect(ctx, source, credential)
	if err != nil {
		return prommodel.QueryResult{}, err
	}
	defer p.http.Close()
	var mapping prommodel.Mapping
	found := false
	for _, m := range p.cfg.Mappings {
		if m.ID == q.MappingID {
			mapping = m
			found = true
		}
	}
	if !found || !p.cfg.Selected(t.ScrapePool, t.Labels) || t.ID != prommodel.TargetID(t.ScrapePool, t.Identity) {
		return prommodel.QueryResult{}, prommodel.ErrInvalid
	}
	result := prommodel.QueryResult{SourceID: source.ID, SourceRevision: source.Revision, AssetID: q.AssetID, TargetID: t.ID, MappingID: mapping.ID, Metric: mapping.Metric, Unit: mapping.Unit, Kind: q.Kind, TimeSemantics: "promql_evaluation", Quality: "complete", Points: []prommodel.Point{}, Metadata: []prommodel.Metadata{}}
	if q.Kind == "metadata" {
		result.Metadata, err = p.metadata(ctx, t, mapping)
	} else {
		to := now
		if q.Kind == "range" {
			to = q.To
		}
		result.Points, result.Quality, err = p.evaluate(ctx, t, mapping, q.Kind, q.From, to, q.StepSeconds)
	}
	return result, err
}
