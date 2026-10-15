package prometheus

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type targetResponse struct {
	Labels         map[string]string `json:"labels"`
	Pool           string            `json:"scrapePool"`
	Health         string            `json:"health"`
	LastScrape     string            `json:"lastScrape"`
	ScrapeInterval string            `json:"scrapeInterval"`
}

func (c *client) clean(value string, limit int) (string, error) {
	if !prommodel.Text(value, limit) {
		return "", datasources.Failure{Code: "invalid_response"}
	}
	v := logbook.SanitizeText(value)
	if len(c.credential) > 0 {
		v = strings.ReplaceAll(v, string(c.credential), "[REDACTED]")
	}
	return v, nil
}
func (c *client) targets(ctx context.Context) ([]prommodel.Target, error) {
	if len(c.cfg.ScrapePools) == 0 {
		return nil, datasources.Failure{Code: "configuration_required"}
	}
	items := []prommodel.Target{}
	seen := map[string]bool{}
	for _, pool := range c.cfg.ScrapePools {
		var response struct {
			Active []targetResponse `json:"activeTargets"`
		}
		if err := c.get(ctx, "api/v1/targets", url.Values{"state": {"active"}, "scrapePool": {pool}}, &response); err != nil {
			return nil, err
		}
		if len(response.Active) > 5000 {
			return nil, datasources.Failure{Code: "response_too_large"}
		}
		for _, raw := range response.Active {
			if raw.Pool != pool {
				return nil, datasources.Failure{Code: "invalid_response"}
			}
			if !c.cfg.Selected(pool, raw.Labels) {
				continue
			}
			if raw.Health != "up" && raw.Health != "down" && raw.Health != "unknown" {
				return nil, datasources.Failure{Code: "invalid_response"}
			}
			identity := map[string]string{}
			labels := map[string]string{}
			for _, k := range c.cfg.IdentityLabels {
				v := raw.Labels[k]
				safe, err := c.clean(v, 512)
				if err != nil || v == "" || safe != v {
					return nil, datasources.Failure{Code: "invalid_response"}
				}
				identity[k] = v
				labels[k] = v
			}
			keys := []string{c.cfg.HostnameLabel, c.cfg.ManufacturerLabel, c.cfg.ModelLabel, c.cfg.SerialLabel, c.cfg.UUIDLabel}
			for k := range c.cfg.LabelMatch {
				keys = append(keys, k)
			}
			for _, k := range keys {
				if k == "" {
					continue
				}
				v, err := c.clean(raw.Labels[k], 512)
				if err != nil {
					return nil, err
				}
				labels[k] = v
			}
			t := prommodel.Target{ID: prommodel.TargetID(pool, identity), ScrapePool: pool, Labels: labels, Identity: identity, Health: raw.Health, SourceRevision: c.source.Revision, APIVersion: c.version}
			if raw.LastScrape != "" && !strings.HasPrefix(raw.LastScrape, "0001-") {
				at, err := time.Parse(time.RFC3339Nano, raw.LastScrape)
				if err != nil || at.Year() < 1970 || at.Year() > 9999 {
					return nil, datasources.Failure{Code: "invalid_response"}
				}
				at = at.UTC()
				t.LastScrape = &at
			}
			interval, err := c.clean(raw.ScrapeInterval, 64)
			if err != nil {
				return nil, err
			}
			t.ScrapeInterval = interval
			if seen[t.ID] {
				return nil, datasources.Failure{Code: "invalid_response"}
			}
			seen[t.ID] = true
			items = append(items, t)
			if len(items) > c.cfg.MaxTargets {
				return nil, datasources.Failure{Code: "response_too_large"}
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
func (c *client) device(t prommodel.Target, now time.Time) *pb.Device {
	types := map[string]pb.DeviceType{"server": pb.DeviceType_DEVICE_TYPE_SERVER, "router": pb.DeviceType_DEVICE_TYPE_ROUTER, "switch": pb.DeviceType_DEVICE_TYPE_SWITCH, "firewall": pb.DeviceType_DEVICE_TYPE_FIREWALL, "hypervisor": pb.DeviceType_DEVICE_TYPE_HYPERVISOR}
	kind, ok := types[c.cfg.DeviceType]
	if !ok {
		kind = pb.DeviceType_DEVICE_TYPE_UNKNOWN
	}
	source := &pb.SourceRef{Type: "prometheus", InstanceId: c.source.ID, TargetId: t.ID, ResourceId: "/targets/" + t.ID, Protocol: "http_api_v1", Provenance: "external"}
	d := &pb.Device{Id: t.ID, Name: t.Labels["job"] + " / " + t.Labels["instance"], Type: kind, Source: "prometheus", Observation: &pb.ObservationMetadata{Source: source, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now), ClockQuality: "unknown"}}}
	if c.cfg.HostnameLabel != "" {
		d.Hostname = t.Labels[c.cfg.HostnameLabel]
	}
	if c.cfg.ManufacturerLabel != "" {
		d.Vendor = t.Labels[c.cfg.ManufacturerLabel]
	}
	if c.cfg.ModelLabel != "" {
		d.Model = t.Labels[c.cfg.ModelLabel]
	}
	if c.cfg.SerialLabel != "" {
		d.SerialNumber = t.Labels[c.cfg.SerialLabel]
	}
	confidence := "medium"
	if c.cfg.VerifiedIdentityLabels {
		confidence = "high"
	}
	for _, fact := range []struct{ kind, value string }{{"serial", d.SerialNumber}, {"hardware_uuid", t.Labels[c.cfg.UUIDLabel]}} {
		if fact.value != "" {
			d.IdentityEvidence = append(d.IdentityEvidence, &pb.IdentityEvidence{Type: fact.kind, Value: fact.value, Source: source, Confidence: confidence})
		}
	}
	// Target health is scrape health, not physical online/power/host identity.
	return d
}
func (c *Connector) sync(ctx context.Context, r datasources.Request) (datasources.Result, error) {
	if r.Source.Configuration.Prometheus == nil {
		return datasources.Result{}, datasources.Failure{Code: "configuration_required"}
	}
	if err := c.acquire(ctx); err != nil {
		return datasources.Result{}, err
	}
	defer c.release()
	p, err := c.connect(ctx, r.Source, r.Credential)
	if err != nil {
		return datasources.Result{}, err
	}
	defer p.http.Close()
	items, err := p.targets(ctx)
	if err != nil {
		return datasources.Result{}, err
	}
	var cursor struct {
		After string `json:"after_target"`
	}
	if len(r.Cursor) > 0 && string(r.Cursor) != "{}" {
		if json.Unmarshal(r.Cursor, &cursor) != nil || cursor.After != "" && !prommodel.TargetIDValid(cursor.After) {
			return datasources.Result{}, datasources.Failure{Code: "invalid_response"}
		}
	}
	page := []prommodel.Target{}
	for _, t := range items {
		if t.ID > cursor.After {
			page = append(page, t)
			if len(page) == p.cfg.PageSize {
				break
			}
		}
	}
	// An exhausted/removed cursor restarts the bounded selected target cycle.
	if len(page) == 0 && len(items) > 0 {
		page = items[:min(p.cfg.PageSize, len(items))]
	}
	now := time.Now().UTC()
	result := datasources.Result{Observations: []assets.Observation{}, Prometheus: &prommodel.Batch{Targets: page}}
	if p.cfg.AlertsEnabled && len(page) > 0 {
		result.Prometheus.Alerts, err = p.alerts(ctx, items, page, now)
		if err != nil {
			return datasources.Result{}, err
		}
	}
	for _, t := range page {
		d := p.device(t, now)
		if err = p.enrich(ctx, t, d); err != nil {
			return datasources.Result{}, err
		}
		raw, _ := json.Marshal(t)
		result.Observations = append(result.Observations, assets.Observation{ExternalType: "host", ExternalID: t.ID, ObservedAt: now, Provenance: "external", Device: d, ProviderState: raw})
		for _, m := range p.cfg.Mappings {
			value, err := p.instant(ctx, t, m, now)
			if err != nil {
				return datasources.Result{}, err
			}
			result.Prometheus.Measurements = append(result.Prometheus.Measurements, value)
		}
	}
	cursor.After = ""
	if len(page) > 0 && page[len(page)-1].ID != items[len(items)-1].ID {
		cursor.After = page[len(page)-1].ID
	}
	result.Cursor, err = json.Marshal(cursor)
	return result, err
}
