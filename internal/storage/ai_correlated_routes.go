package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateCorrelatedPreview(ctx context.Context, id string, cfg analysisai.Config) (analysisai.Preview, error) {
	card, err := s.CorrelatedIncident(ctx, id)
	if err != nil {
		return analysisai.Preview{}, err
	}
	for _, member := range card.Signals {
		rule, _ := member.Signal.Payload["rule_id"].(string)
		if !analysisai.IsSupportedRule(rule) {
			continue
		}
		p, err := s.CreatePreview(ctx, member.Signal.IncidentID, cfg)
		if err != nil {
			return p, err
		}
		if p.CorrelatedIncidentID == nil || *p.CorrelatedIncidentID != id {
			return analysisai.Preview{}, analysisai.ErrConflict
		}
		return p, nil
	}
	return analysisai.Preview{}, analysisai.ErrInvalid
}

func (s *Store) CreateCorrelatedJob(ctx context.Context, id string, body analysisai.Create, key string, cfg analysisai.Config) (analysisai.Job, bool, error) {
	actor, ok := access.Current(ctx)
	if !ok {
		return analysisai.Job{}, false, access.ErrForbidden
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT preview FROM ai_previews WHERE id=$1 AND created_by=$2 AND preview->>'correlated_incident_id'=$3`, body.PreviewID, actor.User.ID, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return analysisai.Job{}, false, analysisai.ErrConflict
	}
	if err != nil {
		return analysisai.Job{}, false, err
	}
	var p analysisai.Preview
	if err = json.Unmarshal(raw, &p); err != nil {
		return analysisai.Job{}, false, err
	}
	return s.CreateJob(ctx, p.IncidentID, body, key, cfg)
}
func (s *Store) CorrelatedAnalysisJobs(ctx context.Context, id string, limit int) ([]analysisai.Job, error) {
	if limit < 1 || limit > 100 {
		return nil, analysisai.ErrInvalid
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM correlated_incidents WHERE id=$1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+aiJobColumns+` FROM ai_analyses WHERE preview_id IN (SELECT id FROM ai_previews WHERE preview->>'correlated_incident_id'=$1) ORDER BY created_at DESC,id LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []analysisai.Job{}
	for rows.Next() {
		v, e := readAIJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
