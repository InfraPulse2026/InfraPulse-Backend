package prometheus

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"sort"
	"strings"
	"time"
)

func (c *client) alerts(ctx context.Context, all, page []prommodel.Target, now time.Time) ([]prommodel.Alert, error) {
	var response struct {
		Alerts []struct {
			Labels   map[string]string `json:"labels"`
			State    string            `json:"state"`
			ActiveAt string            `json:"activeAt"`
		} `json:"alerts"`
	}
	if err := c.get(ctx, "api/v1/alerts", nil, &response); err != nil {
		return nil, err
	}
	if len(response.Alerts) > 1000 {
		return nil, datasources.Failure{Code: "response_too_large"}
	}
	selected := map[string]bool{}
	for _, t := range page {
		selected[t.ID] = true
	}
	out := []prommodel.Alert{}
	seen := map[string]bool{}
	for _, raw := range response.Alerts {
		if raw.State != "firing" && raw.State != "pending" {
			return nil, datasources.Failure{Code: "invalid_response"}
		}
		if raw.State != "firing" {
			continue
		}
		if len(raw.Labels) > 64 {
			return nil, datasources.Failure{Code: "response_too_large"}
		}
		for k, v := range raw.Labels {
			if !prommodel.Text(k, 128) || !prommodel.Text(v, 1024) {
				return nil, datasources.Failure{Code: "invalid_response"}
			}
		}
		matches := []string{}
		for _, t := range all {
			ok := true
			for k, v := range t.Identity {
				if raw.Labels[k] != v {
					ok = false
					break
				}
			}
			if ok {
				matches = append(matches, t.ID)
			}
		}
		// Pool and target ambiguity cannot be solved by guessing from an IP/name.
		if len(matches) != 1 || !selected[matches[0]] {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, raw.ActiveAt)
		if err != nil || at.Year() < 1970 || at.Year() > 9999 || at.After(now.Add(30*time.Second)) {
			return nil, datasources.Failure{Code: "invalid_response"}
		}
		name, err := c.clean(raw.Labels["alertname"], 256)
		if err != nil || strings.TrimSpace(name) == "" {
			return nil, datasources.Failure{Code: "invalid_response"}
		}
		severity := "warning"
		switch strings.ToLower(raw.Labels["severity"]) {
		case "critical", "fatal", "disaster":
			severity = "critical"
		}
		fingerprint := prommodel.Hash("prom_alert_", []any{matches[0], raw.Labels, at.UTC().Format(time.RFC3339Nano)})
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		out = append(out, prommodel.Alert{LabelFingerprint: prommodel.Hash("prom_labels_", raw.Labels), Fingerprint: fingerprint, TargetID: matches[0], Name: name, Severity: severity, ActiveAt: at.UTC()})
		if len(out) > 100 {
			return nil, datasources.Failure{Code: "response_too_large"}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}
