package storage

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/jackc/pgx/v5"
)

func (s *Store) projectInterfaceFlap(ctx context.Context, tx pgx.Tx, agent, device, source, scope, session, iface string, now time.Time, interval int, input []health.ResourceSample) error {
	points := append([]health.ResourceSample(nil), input...)
	sort.Slice(points, func(i, j int) bool {
		if points[i].ObservedAt.Equal(points[j].ObservedAt) {
			return points[i].ID > points[j].ID
		}
		return points[i].ObservedAt.Before(points[j].ObservedAt)
	})
	var epoch, boot, state string
	var observed, baseline time.Time
	var lastChange *time.Time
	err := tx.QueryRow(ctx, `SELECT epoch,boot_key,state,observed_at,baseline_at,last_change_at FROM interface_flap_trackers WHERE agent_id=$1 AND device_id=$2 AND interface_id=$3 AND source=$4 AND scope=$5 FOR UPDATE`, agent, device, iface, source, scope).Scan(&epoch, &boot, &state, &observed, &baseline, &lastChange)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	changed := false
	for _, pt := range points {
		if pt.ObservedAt.After(now) || pt.ObservedAt.After(pt.ReceivedAt.Add(30*time.Second)) || exists && !pt.ObservedAt.After(observed) {
			continue
		}
		next := "unknown"
		if pt.Interface != nil && pt.Interface.Metadata != nil && pt.Interface.Metadata.AdminStatus && pt.Interface.AdminUp {
			switch strings.ToLower(pt.Interface.OperState) {
			case "up":
				next = "up"
			case "down", "lowerlayerdown":
				next = "down"
			}
		}
		bootKey := pt.BootID
		if bootKey == "" {
			bootKey = "session:" + session
		}
		reset := !exists || bootKey != boot || state == "unknown" || next == "unknown" || pt.ObservedAt.Sub(observed) > time.Duration(monitoring.StaleAfterSeconds(interval))*time.Second
		if reset {
			epoch = bootKey + "@" + pt.ObservedAt.UTC().Format(time.RFC3339Nano)
			baseline = pt.ObservedAt
			lastChange = nil
		} else if state != next {
			if _, err = tx.Exec(ctx, `INSERT INTO interface_state_changes(agent_id,device_id,interface_id,source,scope,epoch,telemetry_id,from_state,to_state,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, agent, device, iface, source, scope, epoch, pt.ID, state, next, pt.ObservedAt); err != nil {
				return err
			}
			at := pt.ObservedAt
			lastChange = &at
		}
		boot = bootKey
		state = next
		observed = pt.ObservedAt
		exists = true
		changed = true
	}
	if !changed {
		return nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO interface_flap_trackers(agent_id,device_id,interface_id,source,scope,epoch,boot_key,state,observed_at,baseline_at,last_change_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(agent_id,device_id,interface_id,source,scope) DO UPDATE SET epoch=EXCLUDED.epoch,boot_key=EXCLUDED.boot_key,state=EXCLUDED.state,observed_at=EXCLUDED.observed_at,baseline_at=EXCLUDED.baseline_at,last_change_at=EXCLUDED.last_change_at`, agent, device, iface, source, scope, epoch, boot, state, observed, baseline, lastChange); err != nil {
		return err
	}
	a := health.Assessment{RuleID: "interface.flap", RuleVersion: "1", Policy: s.HealthPolicy(), Status: "unknown", Severity: "none", Reason: "interface_state_unknown", ObservedAt: &observed, Metric: "oper_state_changes", Unit: "changes_in_300_seconds", Evidence: []health.Point{}, FlapWindowSeconds: 300, FlapRecoverySeconds: 300}
	if state != "unknown" {
		rows, e := tx.Query(ctx, `SELECT telemetry_id,observed_at,to_state FROM interface_state_changes WHERE agent_id=$1 AND device_id=$2 AND interface_id=$3 AND source=$4 AND scope=$5 AND epoch=$6 AND observed_at>=$7 ORDER BY observed_at DESC,id DESC LIMIT 1000`, agent, device, iface, source, scope, epoch, observed.Add(-300*time.Second))
		if e != nil {
			return e
		}
		for rows.Next() {
			var pt health.ResourcePoint
			if e = rows.Scan(&pt.RecordID, &pt.ObservedAt, &pt.State); e != nil {
				rows.Close()
				return e
			}
			pt.RecordKind = "telemetry"
			a.ResourceEvidence = append(a.ResourceEvidence, pt)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		count := float64(len(a.ResourceEvidence))
		a.NumericValue = &count
		a.Status = "ok"
		a.Reason = "flap_threshold_not_reached"
		a.WindowSeconds = 300
		if count >= 4 {
			a.Status = "warning"
			a.Severity = "warning"
			a.Reason = "frequent_oper_state_changes"
			if count >= 8 {
				a.Status = "critical"
				a.Severity = "critical"
			}
		}
		quietSince := baseline
		if lastChange != nil {
			quietSince = *lastChange
		}
		a.LastStateChangeAt = lastChange
		stable := health.InterfaceRules(now, points, interval)[0].RecoveryEligible
		a.RecoveryEligible = state == "up" && observed.Sub(quietSince) >= 300*time.Second && stable
		if now.Sub(observed) > time.Duration(monitoring.StaleAfterSeconds(interval))*time.Second {
			a.Status = "stale"
			a.Severity = "none"
			a.RecoveryEligible = false
			a.Reason = "interface_observation_stale"
		}
	}
	return saveResourceHealth(ctx, tx, agent, device, source, scope, "interface", iface, "", a)
}
