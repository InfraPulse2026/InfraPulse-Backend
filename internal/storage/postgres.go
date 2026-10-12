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
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorcontract"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prometheus"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
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

//go:embed migrations/004_topology.sql
var topologySchema string

//go:embed migrations/005_availability.sql
var availabilitySchema string

//go:embed migrations/006_logbook.sql
var logbookSchema string

//go:embed migrations/007_telephony.sql
var telephonySchema string

//go:embed migrations/008_access.sql
var accessSchema string

//go:embed migrations/009_commands.sql
var commandSchema string

//go:embed migrations/010_snapshots_rollback.sql
var rollbackSchema string

//go:embed migrations/011_pki.sql
var pkiSchema string

//go:embed migrations/012_health_incidents.sql
var healthSchema string

//go:embed migrations/013_resource_health.sql
var resourceHealthSchema string

//go:embed migrations/014_flap_collectors.sql
var flapCollectorSchema string

//go:embed migrations/015_reports.sql
var reportSchema string

//go:embed migrations/016_ai_analysis.sql
var aiSchema string

//go:embed migrations/017_collector_metadata.sql
var collectorMetadataSchema string

//go:embed migrations/018_data_sources.sql
var dataSourceSchema string

//go:embed migrations/019_data_source_jobs.sql
var dataSourceJobSchema string

//go:embed migrations/020_asset_registry.sql
var assetRegistrySchema string

//go:embed migrations/021_asset_decisions.sql
var assetDecisionSchema string

//go:embed migrations/022_asset_resolution.sql
var assetResolutionSchema string

//go:embed migrations/023_asset_provider_state.sql
var assetProviderSchema string

//go:embed migrations/024_external_monitoring.sql
var externalMonitoringSchema string

//go:embed migrations/025_prometheus.sql
var prometheusSchema string

//go:embed migrations/026_inventory_locations.sql
var physicalInventorySchema string

//go:embed migrations/027_global_topology.sql
var globalTopologySchema string

//go:embed migrations/028_asset_hardware.sql
var assetHardwareSchema string

//go:embed migrations/029_asset_logs.sql
var assetLogSchema string

//go:embed migrations/030_asset_telephony.sql
var assetVoIPSchema string

//go:embed migrations/031_collector_configuration.sql
var collectorConfigSchema string

//go:embed migrations/032_automation.sql
var automationSchema string

//go:embed migrations/033_configuration_drift.sql
var configurationDriftSchema string

var ErrNotFound = errors.New("not found")
var ErrAmbiguous = errors.New("device ID belongs to multiple agents")
var ErrMessageConflict = errors.New("message ID reused with different contents")

type Store struct {
	configurationDriftMaxAge time.Duration
	automationEnabled        bool
	automationVault          *datasources.Vault
	collectorConfigEnabled   bool
	prometheus               *prometheus.Connector
	autoAssetResolution      bool
	assetResolutionAge       time.Duration
	sourceVault              *datasources.Vault
	sourceOperations         *datasources.Registry
	pool                     *pgxpool.Pool
	timeout                  time.Duration
	defaults                 monitoring.Policy
	telephonyInterval        int
	commandPolicy            commands.Policy
	healthPolicy             health.Policy
	healthClock              func() time.Time // unset in production; deterministic storage fixtures
	collectorStartedAt       time.Time
}

func Open(ctx context.Context, url string, agentTimeout time.Duration, defaults monitoring.Policy, sipInterval ...int) (*Store, error) {
	if err := defaults.Validate(); err != nil {
		return nil, err
	}
	interval := telephony.DefaultIntervalSeconds
	if len(sipInterval) > 1 {
		return nil, errors.New("at most one SIP interval is accepted")
	}
	if len(sipInterval) == 1 {
		interval = sipInterval[0]
	}
	if err := telephony.ValidateInterval(interval); err != nil {
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
	return &Store{pool: p, timeout: agentTimeout, defaults: defaults, telephonyInterval: interval}, nil
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
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=4)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, topologySchema); err != nil {
			return fmt.Errorf("migration 4: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(4)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=5)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, availabilitySchema); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(5)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=6)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, logbookSchema); err != nil {
			return fmt.Errorf("migration 6: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(6)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=7)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, telephonySchema); err != nil {
			return fmt.Errorf("migration 7: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(7)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=8)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, accessSchema); err != nil {
			return fmt.Errorf("migration 8: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(8)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=9)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, commandSchema); err != nil {
			return fmt.Errorf("migration 9: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(9)`); err != nil {
			return err
		}
	}
	// This MVP runs one controller instance. Streams do not survive a restart.
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=10)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, rollbackSchema); err != nil {
			return fmt.Errorf("migration 10: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(10)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=11)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, pkiSchema); err != nil {
			return fmt.Errorf("migration 11: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(11)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=12)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, healthSchema); err != nil {
			return fmt.Errorf("migration 12: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(12)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=13)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, resourceHealthSchema); err != nil {
			return fmt.Errorf("migration 13: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(13)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=14)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, flapCollectorSchema); err != nil {
			return fmt.Errorf("migration 14: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(14)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=15)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, reportSchema); err != nil {
			return fmt.Errorf("migration 15: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(15)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=16)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, aiSchema); err != nil {
			return fmt.Errorf("migration 16: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(16)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=17)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, collectorMetadataSchema); err != nil {
			return fmt.Errorf("migration 17: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(17)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=18)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, dataSourceSchema); err != nil {
			return fmt.Errorf("migration 18: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(18)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=19)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, dataSourceJobSchema); err != nil {
			return fmt.Errorf("migration 19: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(19)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=20)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetRegistrySchema); err != nil {
			return fmt.Errorf("migration 20: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(20)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=21)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetDecisionSchema); err != nil {
			return fmt.Errorf("migration 21: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(21)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=22)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetResolutionSchema); err != nil {
			return fmt.Errorf("migration 22: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(22)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=23)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetProviderSchema); err != nil {
			return fmt.Errorf("migration 23: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(23)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=24)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, externalMonitoringSchema); err != nil {
			return fmt.Errorf("migration 24: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(24)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=25)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, prometheusSchema); err != nil {
			return fmt.Errorf("migration 25: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(25)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=26)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, physicalInventorySchema); err != nil {
			return fmt.Errorf("migration 26: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(26)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=27)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, globalTopologySchema); err != nil {
			return fmt.Errorf("migration 27: %w", err)
		}
		if err = seedGlobalTopology(ctx, tx); err != nil {
			return fmt.Errorf("migration 27 import: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(27)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=28)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetHardwareSchema); err != nil {
			return fmt.Errorf("migration 28: %w", err)
		}
		if err = seedAssetHardware(ctx, tx); err != nil {
			return fmt.Errorf("migration 28 import: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(28)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=29)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetLogSchema); err != nil {
			return fmt.Errorf("migration 29: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(29)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=30)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, assetVoIPSchema); err != nil {
			return fmt.Errorf("migration 30: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(30)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=31)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, collectorConfigSchema); err != nil {
			return fmt.Errorf("migration 31: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(31)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=32)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, automationSchema); err != nil {
			return fmt.Errorf("migration 32: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(32)`); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=33)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, configurationDriftSchema); err != nil {
			return fmt.Errorf("migration 33: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(33)`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE agents SET connected=false`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET disconnected_at=now() WHERE disconnected_at IS NULL`); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	s.collectorStartedAt = time.Now().UTC()
	return nil
}

var jsonOptions = protojson.MarshalOptions{UseProtoNames: true}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

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
	if err := collectorcontract.Validate(e); err != nil {
		return err
	}
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
	if err = projectCollectorConfig(ctx, tx, e); err != nil {
		return err
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
			_, err = tx.Exec(ctx, `UPDATE devices d SET online=CASE WHEN state_at>now()+interval '30 seconds' OR $2>=state_at THEN true ELSE online END,
			state_at=CASE WHEN state_at>now()+interval '30 seconds' THEN $2 ELSE GREATEST(state_at,$2) END,
			last_seen=CASE WHEN d.last_seen>now()+interval '30 seconds' THEN $2 ELSE GREATEST(d.last_seen,$2) END,received_at=now()
			FROM agents a WHERE d.agent_id=a.agent_id AND d.device_id=a.local_device_id AND a.agent_id=$1`, e.AgentId, time.Now().UTC())
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
	case *pb.AgentEnvelope_Topology:
		err = projectTopology(ctx, tx, e)
	case *pb.AgentEnvelope_Telephony:
		err = projectTelephony(ctx, tx, e)
	case *pb.AgentEnvelope_CommandResult:
		err = projectCommandResult(ctx, tx, e)
	case *pb.AgentEnvelope_Logs:
		var received time.Time
		if err = tx.QueryRow(ctx, `SELECT now()`).Scan(&received); err == nil {
			_, err = projectLogs(ctx, tx, e, received)
		}
	}
	if err != nil {
		return err
	}
	if err = projectAvailability(ctx, tx, e); err != nil {
		return err
	}
	if err = s.refreshManagedMonitoring(ctx, tx, e); err != nil {
		return err
	}
	if err = s.projectHealth(ctx, tx, e); err != nil {
		return err
	}
	if err = s.projectResourceHealth(ctx, tx, e); err != nil {
		return err
	}
	if err = s.projectCollectorAssets(ctx, tx, e); err != nil {
		return err
	}
	if err = projectCollectorHardware(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func inventory(ctx context.Context, tx pgx.Tx, agentID string, d *pb.Device, observed time.Time) error {
	if observed.After(time.Now().Add(30 * time.Second)) {
		// Preserve the full envelope, but don't let a future clock poison
		// inventory chronology. Availability separately records the anomaly.
		_, err := tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,source,state_at,last_seen)
		VALUES($1,$2,$2,$3,now(),now()) ON CONFLICT(agent_id,device_id) DO NOTHING`, agentID, d.Id, d.Source)
		return err
	}
	b, err := wireJSON(d)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,hostname,vendor,model,type,management_address,source,online,state_at,inventory_at,last_seen,inventory)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)
	ON CONFLICT(agent_id,device_id) DO UPDATE SET
	name=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.name ELSE devices.name END,
	hostname=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.hostname ELSE devices.hostname END,
	vendor=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.vendor ELSE devices.vendor END,
	model=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.model ELSE devices.model END,
	type=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.type ELSE devices.type END,
	management_address=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.management_address ELSE devices.management_address END,
	source=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.source ELSE devices.source END,
	inventory=CASE WHEN devices.inventory_at IS NULL OR devices.inventory_at>now()+interval '30 seconds' OR EXCLUDED.inventory_at>=devices.inventory_at THEN EXCLUDED.inventory ELSE devices.inventory END,
	inventory_at=CASE WHEN devices.inventory_at>now()+interval '30 seconds' THEN EXCLUDED.inventory_at ELSE GREATEST(devices.inventory_at,EXCLUDED.inventory_at) END,
	online=CASE WHEN devices.state_at>now()+interval '30 seconds' OR EXCLUDED.state_at>=devices.state_at THEN EXCLUDED.online ELSE devices.online END,
	state_at=CASE WHEN devices.state_at>now()+interval '30 seconds' THEN EXCLUDED.state_at ELSE GREATEST(devices.state_at,EXCLUDED.state_at) END,last_seen=CASE WHEN devices.last_seen>now()+interval '30 seconds' THEN EXCLUDED.last_seen ELSE GREATEST(devices.last_seen,EXCLUDED.last_seen) END,received_at=now()`,
		agentID, d.Id, d.Name, d.Hostname, d.Vendor, d.Model, d.Type.String(), d.ManagementAddress, d.Source, d.Online, observed, minTime(at(d.LastSeen, observed), observed), b)
	return err
}

func telemetry(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope, i int, t *pb.TelemetrySample) error {
	_, err := tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,source,online,state_at,last_seen)
	VALUES($1,$2,$2,$3,$5,$4,$4) ON CONFLICT(agent_id,device_id) DO UPDATE SET
	online=CASE WHEN devices.state_at>now()+interval '30 seconds' OR EXCLUDED.state_at>=devices.state_at THEN true ELSE devices.online END,
	state_at=CASE WHEN devices.state_at>now()+interval '30 seconds' THEN EXCLUDED.state_at ELSE GREATEST(devices.state_at,EXCLUDED.state_at) END,last_seen=CASE WHEN devices.last_seen>now()+interval '30 seconds' THEN EXCLUDED.last_seen ELSE GREATEST(devices.last_seen,EXCLUDED.last_seen) END,received_at=now() WHERE EXCLUDED.state_at<=now()+interval '30 seconds'`, e.AgentId, t.DeviceId, t.Source, t.ObservedAt.AsTime(), !t.ObservedAt.AsTime().After(time.Now().Add(30*time.Second)))
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
