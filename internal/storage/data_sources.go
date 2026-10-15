package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ConfigureDataSources is called once before HTTP starts. A wrong or missing key
// fails startup if encrypted credentials already exist, without replacing them.
func (s *Store) ConfigureDataSources(ctx context.Context, vault *datasources.Vault) error {
	rows, err := s.pool.Query(ctx, `SELECT id,credential_ciphertext FROM data_sources WHERE credential_ciphertext IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted); err != nil {
			return err
		}
		secret, e := vault.Open(id, encrypted)
		if e != nil {
			return errors.New("cannot decrypt data source credentials; restore the original DATA_SOURCE_ENCRYPTION_KEY")
		}
		clear(secret)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.sourceVault = vault
	return nil
}

const sourceColumns = `id,type,name,endpoint,connection_ref,enabled,sync_interval_seconds,configuration,revision,(credential_ciphertext IS NOT NULL),connection_status,last_successful_sync,last_error,created_at,updated_at`

func readSource(row interface{ Scan(...any) error }) (datasources.Source, error) {
	var v datasources.Source
	var config []byte
	err := row.Scan(&v.ID, &v.Type, &v.Name, &v.Endpoint, &v.ConnectionRef, &v.Enabled, &v.SyncIntervalSeconds, &config, &v.Revision, &v.HasCredential, &v.ConnectionStatus, &v.LastSuccessfulSync, &v.LastError, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, datasources.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(config, &v.Configuration); err != nil {
		return v, err
	}
	v.Capabilities = datasources.Capabilities(v.Type)
	return v, nil
}

func (s *Store) DataSources(ctx context.Context, limit, offset int) ([]datasources.Source, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, datasources.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sourceColumns+` FROM data_sources ORDER BY created_at,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []datasources.Source{}
	for rows.Next() {
		v, e := readSource(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	refs := []string{}
	for _, v := range items {
		if v.Type == "COLLECTOR" && v.Enabled {
			refs = append(refs, v.ConnectionRef)
		}
	}
	online := map[string]bool{}
	if len(refs) > 0 {
		live, e := s.pool.Query(ctx, `SELECT agent_id FROM agents WHERE agent_id=ANY($1::text[]) AND connected AND last_seen>now()-make_interval(secs=>$2)`, refs, s.timeout.Seconds())
		if e != nil {
			return nil, e
		}
		for live.Next() {
			var id string
			if e = live.Scan(&id); e != nil {
				live.Close()
				return nil, e
			}
			online[id] = true
		}
		err = live.Err()
		live.Close()
		if err != nil {
			return nil, err
		}
	}
	for i := range items {
		items[i].Capabilities = s.sourceOperations.Capabilities(items[i].Type)
		if !items[i].Enabled {
			items[i].ConnectionStatus = "disabled"
		} else if items[i].Type == "COLLECTOR" {
			items[i].ConnectionStatus = "disconnected"
			if online[items[i].ConnectionRef] {
				items[i].ConnectionStatus = "connected"
			}
		}
	}
	return items, nil
}

func (s *Store) DataSource(ctx context.Context, id string) (datasources.Source, error) {
	v, err := readSource(s.pool.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1`, id))
	if err != nil {
		return v, err
	}
	v.Capabilities = s.sourceOperations.Capabilities(v.Type)
	if !v.Enabled {
		v.ConnectionStatus = "disabled"
		return v, nil
	}
	if v.Type != "COLLECTOR" {
		return v, nil
	}
	// Collector liveness is derived from the existing authenticated stream and
	// heartbeat receipt, never from remote device timestamps or a polling interval.
	var online bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE agent_id=$1 AND connected AND last_seen>now()-make_interval(secs=>$2))`, v.ConnectionRef, s.timeout.Seconds()).Scan(&online)
	if online {
		v.ConnectionStatus = "connected"
	} else {
		v.ConnectionStatus = "disconnected"
	}
	return v, err
}

func (s *Store) SaveDataSource(ctx context.Context, id string, in datasources.Input) (datasources.Source, error) {
	var empty datasources.Source
	normal, err := in.Normalize()
	if err != nil {
		return empty, err
	}
	in = normal
	actor, ok := access.Current(ctx)
	if !ok {
		return empty, access.ErrForbidden
	}
	creating := id == ""
	if creating {
		if in.Revision != 0 {
			return empty, datasources.ErrInvalid
		}
		id, err = access.NewID()
		if err != nil {
			return empty, err
		}
		id = "src_" + strings.TrimPrefix(id, "usr_")
	} else if in.Revision < 1 {
		return empty, datasources.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	// Use the same lock as user/session mutations; a revoked administrator cannot
	// commit a source change concurrently with demotion or logout.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return empty, err
	}
	if err = accessAdminTx(ctx, tx, actor); err != nil {
		return empty, err
	}
	var ciphertext []byte
	if !creating {
		var oldType, oldEndpoint, oldConnectionRef string
		var revision int64
		err = tx.QueryRow(ctx, `SELECT type,endpoint,connection_ref,revision,credential_ciphertext FROM data_sources WHERE id=$1 FOR UPDATE`, id).Scan(&oldType, &oldEndpoint, &oldConnectionRef, &revision, &ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, datasources.ErrNotFound
		}
		if err != nil {
			return empty, err
		}
		if revision != in.Revision {
			return empty, datasources.ErrConflict
		}
		// Canonical default port/case changes must not masquerade as moving a
		// credential to another origin during an upgrade from B2.1/B2.2.
		if oldType != in.Type || (oldType == "COLLECTOR" && oldConnectionRef != in.ConnectionRef) || (!datasources.SameEndpoint(oldEndpoint, in.Endpoint) && ciphertext != nil && in.Credential == nil) {
			return empty, datasources.ErrInvalid
		}
	}
	if in.Credential != nil {
		ciphertext = nil
		if *in.Credential != "" {
			ciphertext, err = s.sourceVault.Seal(id, *in.Credential)
			if err != nil {
				return empty, err
			}
		}
	}
	config, err := json.Marshal(in.Configuration)
	if err != nil {
		return empty, err
	}
	status := "adapter_unavailable"
	if in.Type == "COLLECTOR" || len(s.sourceOperations.Capabilities(in.Type)) > 0 {
		status = "unverified"
	}
	if !in.Enabled {
		status = "disabled"
	}
	if creating {
		_, err = tx.Exec(ctx, `INSERT INTO data_sources(id,type,name,endpoint,connection_ref,enabled,sync_interval_seconds,configuration,credential_ciphertext,connection_status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, in.Type, in.Name, in.Endpoint, in.ConnectionRef, in.Enabled, in.SyncIntervalSeconds, config, ciphertext, status)
	} else {
		_, err = tx.Exec(ctx, `UPDATE data_sources SET name=$2,endpoint=$3,connection_ref=$4,enabled=$5,sync_interval_seconds=$6,configuration=$7,credential_ciphertext=$8,connection_status=$9,revision=revision+1,updated_at=now(),last_error='',last_successful_sync=NULL,sync_cursor='{}',sync_failures=0,next_sync_at=now() WHERE id=$1`, id, in.Name, in.Endpoint, in.ConnectionRef, in.Enabled, in.SyncIntervalSeconds, config, ciphertext, status)
	}
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return empty, datasources.ErrConflict
		}
		return empty, err
	}
	v, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1`, id))
	if err != nil {
		return empty, err
	}
	v.Capabilities = s.sourceOperations.Capabilities(v.Type)
	action := "updated"
	if creating {
		action = "created"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO data_source_audit(source_id,actor_id,action,revision) VALUES($1,$2,$3,$4)`, id, actor.User.ID, action, v.Revision); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,'success',$3,$4,$5)`, actor.User.ID, "data_source_"+action, id, access.CurrentMeta(ctx).RequestID, access.CurrentMeta(ctx).Peer); err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
