package storage

import (
	"context"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/diagnosis"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
)

func (s *Store) IncidentContext(ctx context.Context, id string, window, limit int) (diagnosis.Context, error) {
	var empty diagnosis.Context
	if window < 60 || window > 3600 || limit < 1 || limit > 50 {
		return empty, diagnosis.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	now := time.Now().UTC()
	seed, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1`, id))
	if err != nil {
		return empty, err
	}
	if seed.ResourceKind == "external" {
		all := []health.Incident{seed}
		if err = s.incidentFreshnessTx(ctx, tx, all, now); err != nil {
			return empty, err
		}
		result := diagnosis.Build(now, all[0], nil, nil, window, limit, false)
		return result, tx.Commit(ctx)
	}
	end := now
	if seed.ResolvedAt != nil {
		end = *seed.ResolvedAt
	}
	gap := time.Duration(window) * time.Second
	demo := seed.Source == "simulated" || seed.Scope != ""
	rows, err := tx.Query(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE agent_id=$1 AND id<>$2 AND opened_at<=$3 AND COALESCE(resolved_at,$4)>=$5 AND ((source='simulated' OR scope<>'')=$7) AND (NOT $7 OR scope=$8) ORDER BY ABS(EXTRACT(epoch FROM opened_at-$6::timestamptz)),id LIMIT 201`, seed.AgentID, seed.ID, end.Add(gap), now, seed.OpenedAt.Add(-gap), seed.OpenedAt, demo, seed.Scope)
	if err != nil {
		return empty, err
	}
	candidates := []health.Incident{}
	for rows.Next() {
		v, e := readIncident(rows)
		if e != nil {
			rows.Close()
			return empty, e
		}
		candidates = append(candidates, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return empty, err
	}
	truncated := len(candidates) > 200
	if truncated {
		candidates = candidates[:200]
	}
	// Freshness uses the SAME transaction as seed/candidates/topology.
	all := append([]health.Incident{seed}, candidates...)
	if err = s.incidentFreshnessTx(ctx, tx, all, now); err != nil {
		return empty, err
	}
	seed = all[0]
	candidates = all[1:]
	links := []diagnosis.Link{}
	if seed.State != "resolved" && seed.EvidenceStatus == "fresh" && seed.DeviceID != "" && (seed.Signal.Status == "warning" || seed.Signal.Status == "critical") {
		inventory, inventoryTruncated, e := topologyDevices(ctx, tx, seed.AgentID, now.Add(-s.timeout))
		if e != nil {
			return empty, e
		}
		observations, observationTruncated, e := topologyObservations(ctx, tx, seed.AgentID, "", false, now, 500, 0)
		if e != nil {
			return empty, e
		}
		g := topology.Build(now, inventory, observations, nil, "", false, inventoryTruncated)
		truncated = truncated || inventoryTruncated || observationTruncated
		nodes := map[string]topology.Node{}
		for _, node := range g.Nodes {
			nodes[node.ID] = node
		}
		scopes := map[string]string{}
		rows, e := tx.Query(ctx, `SELECT o.edge_id,m.session_id FROM topology_observations o JOIN agent_messages m ON m.agent_id=o.agent_id AND m.message_id=o.message_id WHERE o.agent_id=$1 AND o.source='simulated' AND o.expires_at>$2 ORDER BY o.edge_id LIMIT 1001`, seed.AgentID, now)
		if e != nil {
			return empty, e
		}
		for rows.Next() {
			var edge, session string
			if e = rows.Scan(&edge, &session); e != nil {
				rows.Close()
				return empty, e
			}
			scopes[edge] = session
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return empty, e
		}
		if len(scopes) > 1000 {
			truncated = true
		}
		for _, edge := range g.Edges {
			from, to := nodes[edge.From], nodes[edge.To]
			if !from.Known || !to.Known || edge.Resolution.Status != "resolved" || edge.Resolution.Confidence != "high" {
				continue
			}
			links = append(links, diagnosis.Link{EdgeID: edge.ID, AgentID: edge.AgentID, FromDevice: from.DeviceID, ToDevice: to.DeviceID, LocalInterface: edge.LocalPort.InterfaceID, RemoteInterface: edge.RemotePort.InterfaceID, Source: edge.Source, Scope: scopes[edge.ID], Protocol: edge.Protocol, Confidence: edge.Resolution.Confidence, ObservedAt: edge.ObservedAt, ExpiresAt: edge.ExpiresAt})
		}
	}
	result := diagnosis.Build(now, seed, candidates, links, window, limit, truncated)
	return result, tx.Commit(ctx)
}
