package storage

import (
	"context"
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"time"
)

const telephonyTargetSelect = `SELECT t.agent_id,t.target_id,t.first_received_at,t.last_received_at,
a.connected AND a.last_seen>$3,COALESCE(p.expected_interval_seconds,$4),CASE WHEN p.target_id IS NULL THEN 'backend_default' WHEN p.origin='collector_applied' THEN 'collector_applied' ELSE 'target_override' END,to_jsonb(v),to_jsonb(c)
FROM telephony_targets t JOIN agents a USING(agent_id)
LEFT JOIN telephony_policies p USING(agent_id,target_id)
LEFT JOIN telephony_samples v ON v.id=t.latest_sample_id
LEFT JOIN telephony_samples c ON c.id=t.clock_issue_sample_id
WHERE ($1='' OR t.agent_id=$1) AND ($2='' OR t.target_id=$2)
AND ($5='' OR COALESCE(v.transport,c.transport,'')=$5) AND ($6='' OR COALESCE(v.source,c.source,'')=$6)`

func readSIPTarget(row interface{ Scan(...any) error }, now time.Time) (telephony.Target, error) {
	var t telephony.Target
	var connected bool
	var origin string
	var interval int
	var latest, issue []byte
	err := row.Scan(&t.AgentID, &t.TargetID, &t.FirstReceivedAt, &t.LastReceivedAt, &connected, &interval, &origin, &latest, &issue)
	if err != nil {
		return t, err
	}
	if len(latest) > 0 {
		t.Latest = &telephony.Sample{}
		if err = json.Unmarshal(latest, t.Latest); err != nil {
			return t, err
		}
		t.Latest.ObservedAt, t.Latest.ReceivedAt = t.Latest.ObservedAt.UTC(), t.Latest.ReceivedAt.UTC()
	}
	if len(issue) > 0 {
		t.ClockIssue = &telephony.Sample{}
		if err = json.Unmarshal(issue, t.ClockIssue); err != nil {
			return t, err
		}
		t.ClockIssue.ObservedAt, t.ClockIssue.ReceivedAt = t.ClockIssue.ObservedAt.UTC(), t.ClockIssue.ReceivedAt.UTC()
	}
	display := t.Latest
	if display == nil {
		display = t.ClockIssue
	}
	if display != nil {
		t.Address, t.Transport, t.Source = display.Address, display.Transport, display.Source
	}
	t.FirstReceivedAt, t.LastReceivedAt = t.FirstReceivedAt.UTC(), t.LastReceivedAt.UTC()

	t.Monitoring = telephony.DescribeInterval(t.TargetID, interval, origin)
	t.Health = telephony.Evaluate(now, t.Latest, t.ClockIssue, connected, interval)
	return t, nil
}
func (s *Store) TelephonyTargets(ctx context.Context, f telephony.Filter) ([]telephony.Target, error) {
	now := time.Now().UTC()
	rows, err := s.pool.Query(ctx, telephonyTargetSelect+` ORDER BY t.agent_id,t.target_id LIMIT $7 OFFSET $8`, f.AgentID, f.TargetID, now.Add(-s.timeout), s.telephonyDefault(), f.Transport, f.Source, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []telephony.Target{}
	for rows.Next() {
		t, err := readSIPTarget(rows, now)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}
func (s *Store) TelephonyTarget(ctx context.Context, agent, target string) (telephony.Target, error) {
	items, err := s.TelephonyTargets(ctx, telephony.Filter{AgentID: agent, TargetID: target, Limit: 2})
	if err != nil {
		return telephony.Target{}, err
	}
	if len(items) == 0 {
		return telephony.Target{}, ErrNotFound
	}
	if len(items) > 1 {
		return telephony.Target{}, ErrAmbiguous
	}
	return items[0], nil
}
func (s *Store) TelephonySummary(ctx context.Context, agent string) (telephony.Summary, error) {
	now := time.Now().UTC()
	out := telephony.Summary{GeneratedAt: now}
	// One current row per target; no historical scan. Cards and summary share an evaluator.
	rows, err := s.pool.Query(ctx, telephonyTargetSelect, agent, "", now.Add(-s.timeout), s.telephonyDefault(), "", "")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := readSIPTarget(rows, now)
		if err != nil {
			return out, err
		}
		out.Total++
		switch t.Health.Status {
		case "up":
			out.Up++
		case "degraded":
			out.Degraded++
		case "down":
			out.Down++
		case "stale":
			out.Stale++
		default:
			out.Unknown++
		}
		if t.Health.ClockSkew {
			out.ClockSkew++
		}
		if !t.Health.AgentConnected {
			out.AgentDisconnected++
		}
	}
	return out, rows.Err()
}
func (s *Store) TelephonyHistory(ctx context.Context, f telephony.HistoryFilter) (telephony.HistoryPage, error) {
	out := telephony.HistoryPage{Items: []telephony.Sample{}, Limit: f.Limit}
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,target_id,message_id,address,transport,source,reachable,response_code,latency_ms,observed_at,received_at,consecutive_failures,state,error_category,clock_skew
 FROM telephony_samples WHERE agent_id=$1 AND target_id=$2 AND ($3::timestamptz IS NULL OR observed_at >= $3)
 AND ($4::timestamptz IS NULL OR observed_at < $4) AND ($5::bigint=0 OR id<$5) ORDER BY id DESC LIMIT $6`, f.AgentID, f.TargetID, f.From, f.To, f.BeforeID, f.Limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v telephony.Sample
		if err = rows.Scan(&v.ID, &v.AgentID, &v.TargetID, &v.MessageID, &v.Address, &v.Transport, &v.Source, &v.Reachable, &v.ResponseCode, &v.LatencyMS, &v.ObservedAt, &v.ReceivedAt, &v.ConsecutiveFailures, &v.State, &v.ErrorCategory, &v.ClockSkew); err != nil {
			return out, err
		}
		v.ObservedAt, v.ReceivedAt = v.ObservedAt.UTC(), v.ReceivedAt.UTC()
		out.Items = append(out.Items, v)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > f.Limit {
		out.Items = out.Items[:f.Limit]
		v := out.Items[len(out.Items)-1].ID
		out.NextBeforeID = &v
	}
	return out, nil
}
