package storage

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Opt-in synthetic measurement in the integrationStore disposable schema.
// This is not a throughput SLA or a real hardware/support acceptance test.
func TestMetricStoreSyntheticLoad(t *testing.T) {
	if os.Getenv("INFRAPULSE_METRIC_LOAD") != "1" {
		t.Skip("set INFRAPULSE_METRIC_LOAD=1 with expendable TEST_DATABASE_URL")
	}
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	const devices, batches = 20, 100
	start := time.Now()
	writes := []time.Duration{}
	for batch := 0; batch < batches; batch++ {
		e := sampleEnvelope(now.Add(-10*time.Minute + time.Duration(batch)*3*time.Second))
		e.MessageId = fmt.Sprintf("load-%d", batch)
		e.Sequence = uint64(batch + 2)
		e.SentAt = timestamppb.New(now)
		base := e.GetTelemetry().Samples[0]
		samples := make([]*pb.TelemetrySample, 0, devices)
		for device := 0; device < devices; device++ {
			cpu := float64((device + batch) % 100)
			samples = append(samples, &pb.TelemetrySample{DeviceId: fmt.Sprintf("load-device-%d", device), ObservedAt: base.ObservedAt, CollectedAt: base.CollectedAt, Source: "simulated", Cpu: &pb.CPUStats{UsagePercent: &cpu}, Memory: &pb.MemoryStats{TotalBytes: 8 << 30, UsedBytes: 4 << 30}})
		}
		e.GetTelemetry().Samples = samples
		at := time.Now()
		if err := s.Write(ctx, e); err != nil {
			t.Fatal(err)
		}
		writes = append(writes, time.Since(at))
	}
	writeDuration := time.Since(start)
	reads := []time.Duration{}
	for i := 0; i < 100; i++ {
		at := time.Now()
		page, err := s.QueryRange(ctx, MetricRange{AgentID: "agent-1", DeviceID: fmt.Sprintf("load-device-%d", i%devices), From: now.Add(-11 * time.Minute), To: now, Limit: 1000})
		if err != nil || len(page.Items) != batches || page.Truncated {
			t.Fatal(page, err)
		}
		reads = append(reads, time.Since(at))
	}
	var samples, channels, bytes, envelopeBytes int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT (agent_id,device_id,source)),pg_total_relation_size('telemetry'::regclass),pg_total_relation_size('agent_messages'::regclass) FROM telemetry`).Scan(&samples, &channels, &bytes, &envelopeBytes); err != nil {
		t.Fatal(err)
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i] < writes[j] })
	sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
	t.Logf("synthetic devices=%d envelopes=%d samples=%d native_identity_source_channels=%d payload=CPU/RAM interval=3s write_samples_per_second=%.1f envelope_write_p50=%s envelope_write_p95=%s range_read_p50=%s range_read_p95=%s telemetry_relation_bytes=%d envelope_relation_bytes=%d", devices, batches, samples, channels, float64(samples)/writeDuration.Seconds(), writes[49], writes[94], reads[49], reads[94], bytes, envelopeBytes)
	t.Logf("capacity scenario: 20 devices x 28800 samples/day=576000 samples/day; linear 30-day telemetry estimate=%.0f bytes; small-fixture estimate excludes WAL, vacuum/bloat, backups and other tables; native channels are not full scalar/interface Prometheus series cardinality", float64(bytes)/float64(samples)*576000*30)
}
