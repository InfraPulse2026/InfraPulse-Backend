package storage

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"github.com/jackc/pgx/v5"
	"strconv"
	"time"
)

func (s *Store) AlertmanagerReceiver(ctx context.Context, id string) (prommodel.Receiver, error) {
	v := prommodel.Receiver{SourceID: id}
	err := s.pool.QueryRow(ctx, `SELECT revision,source_revision,enabled,expires_at FROM alertmanager_receivers WHERE source_id=$1`, id).Scan(&v.Revision, &v.SourceRevision, &v.Enabled, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var kind string
		err = s.pool.QueryRow(ctx, `SELECT type FROM data_sources WHERE id=$1`, id).Scan(&kind)
		if err == nil && kind == "PROMETHEUS" {
			return v, nil
		}
		return v, ErrNotFound
	}
	return v, err
}
func (s *Store) SaveAlertmanagerReceiver(ctx context.Context, id string, in prommodel.ReceiverInput) (prommodel.Receiver, error) {
	var out prommodel.Receiver
	if !in.Confirmed || in.ExpectedRevision < 0 || in.TTLSeconds < 300 || in.TTLSeconds > 2592000 || len(in.Reason) < 3 || !prommodel.Text(in.Reason, 1000) {
		return out, prommodel.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return out, err
	}
	source, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return out, err
	}
	if source.Type != "PROMETHEUS" || !source.Enabled || source.Configuration.Prometheus == nil || !source.Configuration.Prometheus.AlertsEnabled {
		return out, prommodel.ErrWebhookConflict
	}
	var revision int64
	err = tx.QueryRow(ctx, `SELECT revision FROM alertmanager_receivers WHERE source_id=$1 FOR UPDATE`, id).Scan(&revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if revision != in.ExpectedRevision {
		return out, prommodel.ErrWebhookConflict
	}
	token, err := access.NewToken()
	if err != nil {
		return out, err
	}
	hash, _ := access.TokenHash(token)
	out = prommodel.Receiver{SourceID: id, Revision: revision + 1, SourceRevision: source.Revision, Enabled: in.Enabled, ExpiresAt: time.Now().UTC().Add(time.Duration(in.TTLSeconds) * time.Second)}
	if _, err = tx.Exec(ctx, `INSERT INTO alertmanager_receivers(source_id,revision,source_revision,enabled,token_hash,expires_at,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(source_id) DO UPDATE SET revision=EXCLUDED.revision,source_revision=EXCLUDED.source_revision,enabled=EXCLUDED.enabled,token_hash=EXCLUDED.token_hash,expires_at=EXCLUDED.expires_at,updated_by=EXCLUDED.updated_by,updated_at=now()`, id, out.Revision, out.SourceRevision, out.Enabled, hash, out.ExpiresAt, actor.User.ID); err != nil {
		return out, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'alertmanager_receiver_rotated','success',$2,$3,$4)`, actor.User.ID, id, meta.RequestID, meta.Peer); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	if in.Enabled {
		out.Token = token
	}
	return out, nil
}
func (s *Store) CheckAlertmanagerToken(ctx context.Context, id, token string) error {
	hash, err := access.TokenHash(token)
	if err != nil {
		return prommodel.ErrWebhookAuth
	}
	var expected []byte
	err = s.pool.QueryRow(ctx, `SELECT r.token_hash FROM alertmanager_receivers r JOIN data_sources d ON d.id=r.source_id WHERE r.source_id=$1 AND r.enabled AND r.expires_at>now() AND r.source_revision=d.revision AND d.enabled AND d.type='PROMETHEUS' AND d.configuration->'prometheus'->>'alerts_enabled'='true'`, id).Scan(&expected)
	if errors.Is(err, pgx.ErrNoRows) {
		return prommodel.ErrWebhookAuth
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(hash, expected) != 1 {
		return prommodel.ErrWebhookAuth
	}
	return nil
}
func (s *Store) ReceiveAlertmanager(ctx context.Context, id, token string, w prommodel.Webhook) (prommodel.WebhookResult, error) {
	var out prommodel.WebhookResult
	if err := w.Validate(time.Now().UTC()); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	// Same source lock as settings/sync fences late revocation and endpoint edits.
	source, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, prommodel.ErrWebhookAuth
		}
		return out, err
	}
	var hash []byte
	var enabled bool
	var expiry time.Time
	var sourceRevision int64
	err = tx.QueryRow(ctx, `SELECT token_hash,enabled,expires_at,source_revision FROM alertmanager_receivers WHERE source_id=$1 FOR UPDATE`, id).Scan(&hash, &enabled, &expiry, &sourceRevision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	supplied, hashErr := access.TokenHash(token)
	if err != nil || hashErr != nil || !enabled || !expiry.After(time.Now()) || sourceRevision != source.Revision || !source.Enabled || source.Type != "PROMETHEUS" || source.Configuration.Prometheus == nil || !source.Configuration.Prometheus.AlertsEnabled || subtle.ConstantTimeCompare(hash, supplied) != 1 {
		return out, prommodel.ErrWebhookAuth
	}
	for _, a := range w.Alerts {
		if a.Status != "resolved" {
			out.Ignored++
			continue
		}
		family := prommodel.Hash("prom_labels_", a.Labels)
		rows, e := tx.Query(ctx, `SELECT p.id,s.payload,s.state FROM prometheus_alert_events p JOIN external_signals s ON s.source_id=p.source_id AND s.event_id=p.id::text WHERE p.source_id=$1 AND p.label_fingerprint=$2 AND p.source_revision=$3 AND s.source_revision=$3 AND p.active_at<=($4::timestamptz+interval '1 microsecond') AND p.first_seen>=($4::timestamptz-interval '30 seconds') AND p.active_at<=$5 ORDER BY p.id FOR UPDATE OF p,s`, id, family, source.Revision, a.StartsAt, a.EndsAt)
		if e != nil {
			return out, e
		}
		type match struct {
			id      int64
			payload []byte
			state   string
		}
		matches := []match{}
		for rows.Next() {
			var v match
			if e = rows.Scan(&v.id, &v.payload, &v.state); e != nil {
				rows.Close()
				return out, e
			}
			matches = append(matches, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		if len(matches) != 1 {
			return out, prommodel.ErrWebhookConflict
		}
		m := matches[0]
		if m.state == "resolved" {
			out.Ignored++
			continue
		}
		var v externalmonitor.Signal
		if e = json.Unmarshal(m.payload, &v); e != nil {
			return out, e
		}
		v.State = "resolved"
		end := a.EndsAt.UTC()
		v.RecoveredAt = &end
		v.RecoveryEventID = strconv.FormatInt(m.id, 10)
		if e = s.projectExternalMonitoring(ctx, tx, source, &externalmonitor.Batch{Signals: []externalmonitor.Signal{v}}); e != nil {
			return out, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO alertmanager_receipts(source_id,event_id,starts_at,ends_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, m.id, a.StartsAt, a.EndsAt); e != nil {
			return out, e
		}
		out.Resolved++
	}
	return out, tx.Commit(ctx)
}
