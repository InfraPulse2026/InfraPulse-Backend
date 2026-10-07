package storage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//go:embed migrations/001_initial.sql
var initialSchema string

//go:embed migrations/002_monitoring_policies.sql
var monitoringSchema string

//go:embed migrations/003_monitoring_24_hours.sql
var dailyMonitoringSchema string

var ErrNotFound = errors.New("not found")
var ErrAmbiguous = errors.New("device ID belongs to multiple agents")
var ErrMessageConflict = errors.New("message ID reused with different contents")

type Store struct {
	pool     *pgxpool.Pool
	timeout  time.Duration
	defaults monitoring.Policy
}

func Open(ctx context.Context, url string, agentTimeout time.Duration, defaults monitoring.Policy) (*Store, error) {
	if err := defaults.Validate(); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = 10
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{pool: p, timeout: agentTimeout, defaults: defaults}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194001)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=1)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, initialSchema); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES (1)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=2)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, monitoringSchema); err != nil {
			return fmt.Errorf("migration 2: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(2)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=3)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, dailyMonitoringSchema); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(3)`); err != nil {
			return err
		}
	}
	// This MVP runs one controller instance. Streams do not survive a restart.
	if _, err = tx.Exec(ctx, `UPDATE agents SET connected=false`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET disconnected_at=now() WHERE disconnected_at IS NULL`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var jsonOptions = protojson.MarshalOptions{UseProtoNames: true}

func wireJSON(m proto.Message) ([]byte, error) { return jsonOptions.Marshal(m) }
func at(ts *timestamppb.Timestamp, fallback time.Time) time.Time {
	if ts == nil {
		return fallback
	}
	return ts.AsTime().UTC()
}

// Ingest persists the inbox and domain effects in one transaction and returns
// success only after commit. A replay may change its transport attempt metadata,
// but reusing a message ID for a different kind or business payload is rejected.
func (s *Store) Ingest(ctx context.Context, e *pb.AgentEnvelope) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := wireJSON(e)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if h := e.GetHello(); h != nil {
		helloJSON, err := wireJSON(h)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agents(agent_id,session_id,name,version,protocol_version,hello,local_device_id)
		VALUES($1,$2,$3,$4,1,$5,$6) ON CONFLICT(agent_id) DO UPDATE SET
		session_id=EXCLUDED.session_id,name=EXCLUDED.name,version=EXCLUDED.version,hello=EXCLUDED.hello,local_device_id=EXCLUDED.local_device_id,heartbeat=NULL,connected=true,last_seen=now()`,
			e.AgentId, e.SessionId, h.Name, h.GetBuild().GetVersion(), helloJSON, h.Device.Id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_sessions(agent_id,session_id,started_at) VALUES($1,$2,$3)
		ON CONFLICT(agent_id,session_id) DO UPDATE SET last_seen=now(),disconnected_at=NULL`, e.AgentId, e.SessionId, h.StartedAt.AsTime())
		if err != nil {
			return err
		}
	} else {
		result, err := tx.Exec(ctx, `UPDATE agents SET last_seen=now(),connected=true WHERE agent_id=$1 AND session_id=$2`, e.AgentId, e.SessionId)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return errors.New("agent session is not registered")
		}
		if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET last_seen=now() WHERE agent_id=$1 AND session_id=$2`, e.AgentId, e.SessionId); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `INSERT INTO agent_messages(agent_id,message_id,session_id,sequence,sent_at,kind,payload)
	VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, e.AgentId, e.MessageId, e.SessionId, strconv.FormatUint(e.Sequence, 10), e.SentAt.AsTime(), Kind(e), raw)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		var identical bool
		// Strip only outer attempt metadata. Nested business timestamps and
		// session IDs remain part of the comparison. Compare archived JSONB
		// directly so rows written before application ACK support need no
		// migration, fingerprint backfill, or lossy protobuf re-decoding.
		if err = tx.QueryRow(ctx, `SELECT kind=$4 AND
		(payload - ARRAY['sequence','sent_at','session_id']) =
		($3::jsonb - ARRAY['sequence','sent_at','session_id'])
		FROM agent_messages WHERE agent_id=$1 AND message_id=$2`, e.AgentId, e.MessageId, raw, Kind(e)).Scan(&identical); err != nil {
			return err
		}
		if !identical {
			slog.WarnContext(ctx, "duplicate message conflict", "agent_id", e.AgentId, "message_id", e.MessageId, "kind", Kind(e))
			return ErrMessageConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		slog.DebugContext(ctx, "duplicate message accepted", "agent_id", e.AgentId, "message_id", e.MessageId, "kind", Kind(e))
		return nil
	}
	switch p := e.Payload.(type) {
	case *pb.AgentEnvelope_Hello:
		// Prefer the device's observation time to the transport's send time.
		err = inventory(ctx, tx, e.AgentId, p.Hello.Device, at(p.Hello.Device.LastSeen, e.SentAt.AsTime()))
	case *pb.AgentEnvelope_Heartbeat:
		var b []byte
		b, err = wireJSON(p.Heartbeat)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE agents SET heartbeat=$2 WHERE agent_id=$1`, e.AgentId, b)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE devices d SET online=CASE WHEN $2>=state_at THEN true ELSE online END,
			state_at=GREATEST(state_at,$2),last_seen=GREATEST(d.last_seen,$2),received_at=now()
			FROM agents a WHERE d.agent_id=a.agent_id AND d.device_id=a.local_device_id AND a.agent_id=$1`, e.AgentId, p.Heartbeat.Timestamp.AsTime())
		}
	case *pb.AgentEnvelope_Inventory:
		for _, d := range p.Inventory.Devices {
			err = inventory(ctx, tx, e.AgentId, d, p.Inventory.ObservedAt.AsTime())
			if err != nil {
				break
			}
		}
	case *pb.AgentEnvelope_Telemetry:
		for i, t := range p.Telemetry.Samples {
			err = telemetry(ctx, tx, e, i, t)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func inventory(ctx context.Context, tx pgx.Tx, agentID string, d *pb.Device, observed time.Time) error {
	b, err := wireJSON(d)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,hostname,vendor,model,type,management_address,source,online,state_at,inventory_at,last_seen,inventory)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)
	ON CONFLICT(agent_id,device_id) DO UPDATE SET
	name=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.name ELSE devices.name END,
	hostname=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.hostname ELSE devices.hostname END,
	vendor=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.vendor ELSE devices.vendor END,
	model=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.model ELSE devices.model END,
	type=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.type ELSE devices.type END,
	management_address=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.management_address ELSE devices.management_address END,
	source=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.source ELSE devices.source END,
	inventory=CASE WHEN devices.inventory_at IS NULL OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.inventory ELSE devices.inventory END,
	inventory_at=GREATEST(devices.inventory_at,EXCLUDED.inventory_at),
	online=CASE WHEN EXCLUDED.state_at>=devices.state_at THEN EXCLUDED.online ELSE devices.online END,
	state_at=GREATEST(devices.state_at,EXCLUDED.state_at),last_seen=GREATEST(devices.last_seen,EXCLUDED.last_seen),received_at=now()`,
		agentID, d.Id, d.Name, d.Hostname, d.Vendor, d.Model, d.Type.String(), d.ManagementAddress, d.Source, d.Online, observed, at(d.LastSeen, observed), b)
	return err
}

func telemetry(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope, i int, t *pb.TelemetrySample) error {
	_, err := tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,source,online,state_at,last_seen)
	VALUES($1,$2,$2,$3,true,$4,$4) ON CONFLICT(agent_id,device_id) DO UPDATE SET
	online=CASE WHEN EXCLUDED.state_at>=devices.state_at THEN true ELSE devices.online END,
	state_at=GREATEST(devices.state_at,EXCLUDED.state_at),last_seen=GREATEST(devices.last_seen,EXCLUDED.last_seen),received_at=now()`, e.AgentId, t.DeviceId, t.Source, t.ObservedAt.AsTime())
	if err != nil {
		return err
	}
	b, err := wireJSON(t)
	if err != nil {
		return err
	}
	var cpu *float64
	var total, used *string
	if t.Cpu != nil {
		cpu = t.Cpu.UsagePercent
	}
	if t.Memory != nil {
		a, b := strconv.FormatUint(t.Memory.TotalBytes, 10), strconv.FormatUint(t.Memory.UsedBytes, 10)
		total, used = &a, &b
	}
	_, err = tx.Exec(ctx, `INSERT INTO telemetry(agent_id,device_id,message_id,sample_index,observed_at,collected_at,source,cpu_usage_percent,memory_total_bytes,memory_used_bytes,payload)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, e.AgentId, t.DeviceId, e.MessageId, i, t.ObservedAt.AsTime(), t.CollectedAt.AsTime(), t.Source, cpu, total, used, b)
	return err
}

func Kind(e *pb.AgentEnvelope) string {
	field := e.ProtoReflect().WhichOneof(e.ProtoReflect().Descriptor().Oneofs().ByName("payload"))
	if field == nil {
		return ""
	}
	return string(field.Name())
}

func (s *Store) Disconnect(ctx context.Context, agentID, sessionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `UPDATE agents SET connected=false WHERE agent_id=$1 AND session_id=$2`, agentID, sessionID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET disconnected_at=now() WHERE agent_id=$1 AND session_id=$2`, agentID, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
