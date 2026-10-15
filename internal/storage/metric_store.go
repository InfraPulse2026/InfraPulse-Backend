package storage

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
)

var ErrMetricRange = errors.New("invalid metric range")

// MetricStore preserves the Collector envelope delivery contract. Write must
// commit the receipt and all samples atomically; a TSDB replacement needs an
// outbox and cannot acknowledge a partially written envelope.
type MetricStore interface {
	Write(context.Context, *pb.AgentEnvelope) error
	Latest(context.Context, string, string) (Telemetry, error)
	QueryRange(context.Context, MetricRange) (MetricPage, error)
}

type MetricRange struct {
	AgentID  string    `json:"agent_id"`
	DeviceID string    `json:"device_id"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Limit    int       `json:"limit"`
}

func (r MetricRange) Normalize(now time.Time) (MetricRange, error) {
	r.From, r.To = r.From.UTC().Truncate(time.Microsecond), r.To.UTC().Truncate(time.Microsecond)
	if r.Limit == 0 {
		r.Limit = 1000
	}
	for _, id := range []string{r.AgentID, r.DeviceID} {
		if id == "" || len(id) > 1024 || !utf8.ValidString(id) || strings.ContainsFunc(id, unicode.IsControl) {
			return r, ErrMetricRange
		}
	}
	if r.From.IsZero() || !r.To.After(r.From) || r.To.Sub(r.From) > 31*24*time.Hour || r.To.After(now) || r.Limit < 1 || r.Limit > 1000 {
		return r, ErrMetricRange
	}
	return r, nil
}

type MetricPage struct {
	Items     []Telemetry `json:"items"`
	Range     MetricRange `json:"range"`
	Truncated bool        `json:"truncated"`
}

var _ MetricStore = (*Store)(nil)

func (s *Store) Write(ctx context.Context, envelope *pb.AgentEnvelope) error {
	if envelope == nil || envelope.GetTelemetry() == nil {
		return ErrMetricRange
	}
	return s.Ingest(ctx, envelope)
}

func (s *Store) Latest(ctx context.Context, agentID, deviceID string) (Telemetry, error) {
	if agentID == "" || deviceID == "" {
		return Telemetry{}, ErrMetricRange
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	items, err := s.Metrics(ctx, agentID, deviceID, 1)
	if err != nil {
		return Telemetry{}, err
	}
	if len(items) == 0 {
		return Telemetry{}, ErrNotFound
	}
	return items[0], nil
}

// QueryRange returns raw samples in [from,to), ordered by observed_at/id.
// It deliberately does not fabricate zeroes or silently downsample gaps.
func (s *Store) QueryRange(ctx context.Context, input MetricRange) (MetricPage, error) {
	r, err := input.Normalize(time.Now().UTC())
	page := MetricPage{Items: []Telemetry{}, Range: r}
	if err != nil {
		return page, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,device_id,observed_at,collected_at,received_at,source,cpu_usage_percent,memory_total_bytes::text,memory_used_bytes::text,payload FROM telemetry WHERE agent_id=$1 AND device_id=$2 AND observed_at >= $3 AND observed_at < $4 ORDER BY observed_at,id LIMIT $5`, r.AgentID, r.DeviceID, r.From, r.To, r.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Telemetry
		if err = rows.Scan(&v.ID, &v.AgentID, &v.DeviceID, &v.ObservedAt, &v.CollectedAt, &v.ReceivedAt, &v.Source, &v.CPUUsagePercent, &v.MemoryTotalBytes, &v.MemoryUsedBytes, &v.Sample); err != nil {
			return page, err
		}
		v.ObservedAt, v.CollectedAt, v.ReceivedAt = v.ObservedAt.UTC(), v.CollectedAt.UTC(), v.ReceivedAt.UTC()
		page.Items = append(page.Items, v)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > r.Limit {
		page.Items = page.Items[:r.Limit]
		page.Truncated = true
	}
	return page, nil
}
