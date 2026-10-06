// Package monitoring separates collection cadence from agent connectivity.
package monitoring

import (
	"fmt"
	"time"
)

const MinIntervalSeconds = 3
const MaxIntervalSeconds = 24 * 60 * 60

type Policy struct {
	TelemetryIntervalSeconds int `json:"telemetry_interval_seconds"`
	InventoryIntervalSeconds int `json:"inventory_interval_seconds"`
	TopologyIntervalSeconds  int `json:"topology_interval_seconds"`
}

func Defaults() Policy { return Policy{3, 300, 300} }

func (p Policy) Validate() error {
	for name, value := range map[string]int{"telemetry_interval_seconds": p.TelemetryIntervalSeconds, "inventory_interval_seconds": p.InventoryIntervalSeconds, "topology_interval_seconds": p.TopologyIntervalSeconds} {
		if value < MinIntervalSeconds || value > MaxIntervalSeconds {
			return fmt.Errorf("%s must be an integer between %d and %d", name, MinIntervalSeconds, MaxIntervalSeconds)
		}
	}
	return nil
}

type Settings struct {
	Policy
	Origin                        string `json:"origin"`
	DeliveryMode                  string `json:"delivery_mode"`
	ApplicationStatus             string `json:"application_status"`
	LocalTelemetryYAML            string `json:"local_telemetry_yaml"`
	RemotePollingYAML             string `json:"remote_polling_yaml"`
	RecommendedTopologyTTLSeconds int    `json:"recommended_topology_ttl_seconds"`
}

func Describe(p Policy, origin string) Settings {
	return Settings{Policy: p, Origin: origin, DeliveryMode: "manual_agent_yaml", ApplicationStatus: "unverified",
		LocalTelemetryYAML: fmt.Sprintf("telemetry:\n  fast_interval: %ds\n  inventory_interval: %ds\n  topology_interval: %ds\n  heartbeat_interval: 10s\n", p.TelemetryIntervalSeconds, p.InventoryIntervalSeconds, p.TopologyIntervalSeconds),
		RemotePollingYAML:  fmt.Sprintf("polling:\n  fast_interval: %ds\n  inventory_interval: %ds\n  topology_interval: %ds\n", p.TelemetryIntervalSeconds, p.InventoryIntervalSeconds, p.TopologyIntervalSeconds),
		// Agent v1 rejects remote.topology_ttl greater than 24 hours.
		RecommendedTopologyTTLSeconds: min(MaxIntervalSeconds, StaleAfterSeconds(p.TopologyIntervalSeconds)),
	}
}

// Tolerate two collection intervals plus bounded scheduling/network jitter.
func StaleAfterSeconds(interval int) int { return interval*2 + min(30, max(5, interval/10)) }

type Freshness struct {
	Status                  string     `json:"status"`
	ExpectedIntervalSeconds int        `json:"expected_interval_seconds"`
	StaleAfterSeconds       int        `json:"stale_after_seconds"`
	AgeSeconds              *int64     `json:"age_seconds"`
	ObservedAt              *time.Time `json:"observed_at"`
	ReceivedAt              *time.Time `json:"received_at"`
	ExpectedNextAt          *time.Time `json:"expected_next_at"`
}

func Evaluate(now time.Time, observed, received *time.Time, interval int) Freshness {
	f := Freshness{Status: "no_data", ExpectedIntervalSeconds: interval, StaleAfterSeconds: StaleAfterSeconds(interval)}
	if observed == nil {
		return f
	}
	o := observed.UTC()
	f.ObservedAt = &o
	if received != nil {
		r := received.UTC()
		f.ReceivedAt = &r
	}
	next := o.Add(time.Duration(interval) * time.Second)
	f.ExpectedNextAt = &next
	age := now.Sub(o)
	if age < -30*time.Second {
		f.Status = "clock_skew"
		return f
	}
	seconds := max(int64(0), int64(age/time.Second))
	f.AgeSeconds = &seconds
	f.Status = "fresh"
	if age > time.Duration(f.StaleAfterSeconds)*time.Second {
		f.Status = "stale"
	}
	return f
}
