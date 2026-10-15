package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetricStoreAtomicReplayRangeAndIsolation(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Second)
	for _, id := range []string{"agent-1", "agent-2"} {
		if err := s.Ingest(ctx, registration(id, now)); err != nil {
			t.Fatal(err)
		}
	}
	e := sampleEnvelope(now)
	var store MetricStore = s
	if err := store.Write(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, registration("agent-1", now)); !errors.Is(err, ErrMetricRange) {
		t.Fatal("nontelemetry write", err)
	}
	latest, err := store.Latest(ctx, "agent-1", "shared-router")
	if err != nil || latest.CPUUsagePercent == nil || *latest.CPUUsagePercent != 0 || *latest.MemoryTotalBytes != "18446744073709551615" {
		t.Fatal(latest, err)
	}
	for _, tc := range []struct {
		agent    string
		from, to time.Time
		want     int
	}{{"agent-1", now, now.Add(time.Second), 1}, {"agent-1", now.Add(-time.Second), now, 0}, {"agent-2", now, now.Add(time.Second), 0}} {
		page, err := store.QueryRange(ctx, MetricRange{AgentID: tc.agent, DeviceID: "shared-router", From: tc.from, To: tc.to})
		if err != nil || len(page.Items) != tc.want {
			t.Fatal(page, err)
		}
	}
	if _, err := store.QueryRange(ctx, MetricRange{DeviceID: "shared-router", From: now, To: now.Add(time.Second)}); !errors.Is(err, ErrMetricRange) {
		t.Fatal("unqualified identity accepted", err)
	}
}
