package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

// ConfigureRemediation runs only at startup. A disabled deployment continues to
// support B10/B11.1 without the unnumbered, deliberately unapplied schema.
func (s *Store) ConfigureRemediation(ctx context.Context, policy rm.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(policy)
	if err := json.Unmarshal(raw, &s.remediationPolicy); err != nil {
		return err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('remediation_proposals') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	s.remediationReady = exists
	if policy.Enabled && (!exists || !s.automationEnabled || s.automationVault == nil) {
		return rm.ErrUnavailable
	}
	return nil
}

func (s *Store) remediationPolicyDigest() string {
	raw, _ := json.Marshal(struct {
		Policy              string
		Devices, Interfaces []string
	}{s.remediationPolicy.Digest(), s.commandPolicy.ProtectedDevices, s.commandPolicy.ProtectedInterfaces})
	return au.Digest(string(raw))
}

const remediationColumns = `id,asset_id,drift_id,action,digest,preview,created_by,created_at,expires_at,state,COALESCE(approved_by,''),approved_at,COALESCE(approved_digest,''),COALESCE(job_id,''),COALESCE(before_snapshot_id,''),COALESCE(after_snapshot_id,''),verification,error_code`

func readRemediation(row interface{ Scan(...any) error }) (rm.Proposal, error) {
	var p rm.Proposal
	var action, preview, verification []byte
	var asset string
	var drift int64
	err := row.Scan(&p.ID, &asset, &drift, &action, &p.Digest, &preview, &p.CreatedBy, &p.CreatedAt, &p.ExpiresAt, &p.State, &p.ApprovedBy, &p.ApprovedAt, &p.ApprovedDigest, &p.JobID, &p.BeforeSnapshotID, &p.AfterSnapshotID, &verification, &p.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(action, &p.Action); err != nil {
		return p, err
	}
	var meta struct {
		AssetName                            string
		ActualObservedAt, ExpectedObservedAt time.Time
		Warnings                             []string
	}
	if err = json.Unmarshal(preview, &meta); err != nil {
		return p, err
	}
	p.AssetName, p.ActualObservedAt, p.ExpectedObservedAt, p.Warnings = meta.AssetName, meta.ActualObservedAt, meta.ExpectedObservedAt, meta.Warnings
	if len(verification) > 0 {
		if err = json.Unmarshal(verification, &p.Verification); err != nil {
			return p, err
		}
	}
	if p.Action.AssetID != asset || p.Action.DriftID != drift || rm.Fingerprint(p.Action) != p.Digest {
		return p, rm.ErrConflict
	}
	return p, nil
}
func (s *Store) Remediation(ctx context.Context, id string) (rm.Proposal, error) {
	if !au.Identifier(id) {
		return rm.Proposal{}, rm.ErrInvalid
	}
	if !s.remediationReady {
		return rm.Proposal{}, rm.ErrUnavailable
	}
	if err := s.remediationReadActor(ctx); err != nil {
		return rm.Proposal{}, err
	}
	return readRemediation(s.pool.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1`, id))
}
func (s *Store) remediationReadActor(ctx context.Context) error {
	p, ok := access.Current(ctx)
	if !ok {
		return access.ErrForbidden
	}
	var valid bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions ses ON ses.user_id=u.id WHERE u.id=$1 AND ses.token_hash=$2 AND NOT u.disabled AND u.role IN ('viewer','operator','admin') AND ses.expires_at>now())`, p.User.ID, p.SessionHash).Scan(&valid)
	if err == nil && !valid {
		return access.ErrForbidden
	}
	return err
}
func (s *Store) RemediationHistory(ctx context.Context, id string, limit, offset int) ([]rm.HistoryEvent, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, rm.ErrInvalid
	}
	p, err := s.Remediation(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,COALESCE(actor_id,''),action,outcome,resource,request_id,at FROM access_audit WHERE resource=$1 OR ($2<>'' AND resource=$2) ORDER BY id LIMIT $3 OFFSET $4`, p.ID, p.JobID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []rm.HistoryEvent{}
	for rows.Next() {
		var e rm.HistoryEvent
		if err = rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.Outcome, &e.Resource, &e.RequestID, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Load canonical inventory, not a name supplied by a caller. Reviewed policy
// supplies the access role; live inventory and topology may veto that review.
func (s *Store) remediationIdentityTx(ctx context.Context, tx pgx.Tx, asset, iface string) (rm.Action, string, error) {
	var a rm.Action
	a.AssetID = asset
	a.Interface = iface
	var lifecycle, scope, vendor, name string
	err := tx.QueryRow(ctx, `SELECT revision,lifecycle_status,scope,vendor,canonical_name,COALESCE(canonical_binding_id,'') FROM assets WHERE id=$1 FOR SHARE`, asset).Scan(&a.AssetRevision, &lifecycle, &scope, &vendor, &name, &a.BindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, name, ErrNotFound
	}
	if err != nil {
		return a, name, err
	}
	if lifecycle != "observed" || scope != "" || a.BindingID == "" || !strings.EqualFold(vendor, "Cisco") {
		return a, name, rm.ErrPolicy
	}
	var raw []byte
	var bindingScope, provenance, external, legacy, legacyAgent, sourceID, sourceType string
	var at time.Time
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT b.inventory,b.inventory_at,b.scope,b.provenance,b.external_id,COALESCE(b.legacy_device_id,''),COALESCE(b.legacy_agent_id,''),s.enabled,b.source_instance_id,s.type FROM asset_source_bindings b JOIN data_sources s ON s.id=b.source_instance_id WHERE b.id=$1 AND b.asset_id=$2 FOR SHARE OF b,s`, a.BindingID, asset).Scan(&raw, &at, &bindingScope, &provenance, &external, &legacy, &legacyAgent, &enabled, &sourceID, &sourceType)
	if err != nil {
		return a, name, err
	}
	now := time.Now()
	if !enabled || bindingScope != "" || provenance == "simulated" || at.Before(now.Add(-cd.DefaultMaxAge)) || at.After(now.Add(30*time.Second)) || len(raw) > au.MaxConfiguration {
		return a, name, rm.ErrPolicy
	}
	d := &pb.Device{}
	if protojson.Unmarshal(raw, d) != nil || d.Type != pb.DeviceType_DEVICE_TYPE_SWITCH || !strings.EqualFold(d.Vendor, "Cisco") {
		return a, name, rm.ErrPolicy
	}
	switch strings.ToLower(strings.TrimSpace(d.Os)) {
	case "ios", "ios-xe", "cisco ios", "cisco ios xe", "cisco.ios.ios":
	default:
		return a, name, rm.ErrPolicy
	}
	if legacyAgent == "" || legacy == "" {
		return a, name, rm.ErrPolicy
	}
	var telemetryRaw []byte
	var telemetryAt time.Time
	err = tx.QueryRow(ctx, `SELECT payload,observed_at FROM telemetry WHERE agent_id=$1 AND device_id=$2 ORDER BY observed_at DESC,id DESC LIMIT 1`, legacyAgent, legacy).Scan(&telemetryRaw, &telemetryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, name, rm.ErrPolicy
	}
	if err != nil {
		return a, name, err
	}
	sample := &pb.TelemetrySample{}
	if len(telemetryRaw) > au.MaxConfiguration || protojson.Unmarshal(telemetryRaw, sample) != nil || sample.DeviceId != legacy || sample.Source == "" || sample.Source == "simulated" || telemetryAt.Before(now.Add(-cd.DefaultMaxAge)) || telemetryAt.After(now.Add(30*time.Second)) {
		return a, name, rm.ErrPolicy
	}
	for _, protected := range s.commandPolicy.ProtectedDevices {
		for _, id := range []string{asset, d.Id, external, legacy} {
			if id != "" && sameResource(protected, id) {
				return a, name, rm.ErrPolicy
			}
		}
	}
	var port *pb.Interface
	for _, candidate := range sample.Interfaces {
		if candidate.Name == iface {
			if port != nil {
				return a, name, rm.ErrPolicy
			}
			port = candidate
		}
	}
	if port == nil || port.Id == "" || port.Type == nil || *port.Type != 6 {
		return a, name, rm.ErrPolicy
	}
	for _, other := range sample.Interfaces {
		if other != port && (other.Id == port.Id || other.Name == port.Name) {
			return a, name, rm.ErrPolicy
		}
	}
	a.InterfaceID = port.Id
	aliases := []string{port.Id, port.Name, port.Alias, port.Mac}
	for _, protected := range s.commandPolicy.ProtectedInterfaces {
		for _, alias := range aliases {
			if alias != "" && sameResource(protected, alias) {
				return a, name, rm.ErrPolicy
			}
		}
	}
	mgmt := managementIP(d.ManagementAddress)
	for _, address := range port.Addresses {
		ip := net.ParseIP(address)
		if strings.Contains(address, "/") {
			ip, _, _ = net.ParseCIDR(address)
		}
		if mgmt != nil && ip != nil && mgmt.Equal(ip) {
			return a, name, rm.ErrPolicy
		}
	}
	// Any observed L2 neighbor makes this unsuitable for the access-port MVP.
	rows, e := tx.Query(ctx, `SELECT l.payload FROM global_topology_latest l JOIN asset_source_bindings b ON b.source_instance_id=l.source_instance_id AND b.external_id=l.device_id AND b.scope=l.scope WHERE b.asset_id=$1 AND l.scope='' AND l.expires_at>now() ORDER BY l.version_id LIMIT 1001`, asset)
	if e != nil {
		return a, name, e
	}
	n := 0
	for rows.Next() {
		n++
		var payload []byte
		if e = rows.Scan(&payload); e != nil {
			break
		}
		neighbor := &pb.Neighbor{}
		if len(payload) > 65536 || protojson.Unmarshal(payload, neighbor) != nil {
			e = rm.ErrPolicy
			break
		}
		resolved := false
		for _, candidate := range sample.Interfaces {
			matched := neighbor.LocalInterfaceIndex != nil && candidate.Index > 0 && candidate.GetMetadata().GetIndex() && uint32(candidate.Index) == *neighbor.LocalInterfaceIndex
			for _, alias := range []string{candidate.Id, candidate.Name, candidate.Alias, candidate.Mac} {
				if alias != "" && (sameResource(alias, neighbor.LocalInterfaceId) || sameResource(alias, neighbor.LocalInterfaceName) || sameResource(alias, neighbor.LocalInterfaceAlias) || sameResource(alias, neighbor.LocalInterfaceMac) || (neighbor.LocalPortSubtype == 5 && alias == string(neighbor.LocalPortId))) {
					matched = true
				}
			}
			if matched {
				resolved = true
				if candidate == port {
					e = rm.ErrPolicy
					break
				}
			}
		}
		// LLDP LocalPortNumber is never inferred to be an ifIndex.
		if !resolved {
			e = rm.ErrPolicy
		}
		if e != nil {
			break
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return a, name, e
	}
	if n > 1000 {
		return a, name, rm.ErrPolicy
	}
	fingerprint, _ := json.Marshal(struct {
		Binding, External, Legacy, Device, Vendor, Management, ID, Name, Alias, MAC, Description, OS, Model, Software, SourceID, SourceType, LegacyAgent string
		Type                                                                                                                                             uint32
		Addresses                                                                                                                                        []string
		AdminUp                                                                                                                                          bool
	}{a.BindingID, external, legacy, d.Id, d.Vendor, d.ManagementAddress, port.Id, port.Name, port.Alias, port.Mac, port.Description, d.Os, d.Model, d.SoftwareVersion, sourceID, sourceType, legacyAgent, *port.Type, port.Addresses, port.AdminUp})
	indexRaw, _ := json.Marshal(struct {
		Index int32
		Known bool
	}{port.Index, port.GetMetadata().GetIndex()})
	a.BindingDigest = au.Digest(string(fingerprint) + string(indexRaw))
	var target au.Target
	var definition []byte
	err = tx.QueryRow(ctx, `SELECT revision,definition FROM automation_targets WHERE asset_id=$1 FOR SHARE`, asset).Scan(&target.Revision, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, name, ErrNotFound
	}
	if err != nil {
		return a, name, err
	}
	if json.Unmarshal(definition, &target.TargetInput) != nil || target.Validate() != nil || !target.Enabled || target.AssetRevision != a.AssetRevision || target.TemplateID != "cisco_ios_read_config_v1" {
		return a, name, rm.ErrPolicy
	}
	target.AssetID = asset
	if mgmt == nil || mgmt.String() != target.Host {
		return a, name, rm.ErrPolicy
	}
	a.TargetRevision = target.Revision
	targetRaw, _ := json.Marshal(target.TargetInput)
	a.TargetDigest = au.Digest(string(targetRaw))
	a.PolicyDigest = s.remediationPolicyDigest()
	a.TemplateID = au.RestoreTemplate().ID
	a.TemplateDigest = au.RestoreTemplateDigest()
	a.ActionType = "restore_access_vlan"
	return a, name, nil
}

func (s *Store) CreateRemediation(ctx context.Context, asset string, in rm.CreateInput, key string) (rm.Proposal, bool, error) {
	p, replay, err := s.createRemediation(ctx, asset, in, key)
	if err != nil {
		s.recordRemediationDenial(ctx, asset, err)
	}
	return p, replay, err
}
func (s *Store) createRemediation(ctx context.Context, asset string, in rm.CreateInput, key string) (rm.Proposal, bool, error) {
	var out rm.Proposal
	if !s.remediationReady || !s.remediationPolicy.Enabled || !s.automationEnabled {
		return out, false, rm.ErrUnavailable
	}
	if !au.Identifier(asset) || in.Validate() != nil || commands.ValidateKey(key) != nil {
		return out, false, rm.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, false); err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194006)`); err != nil {
		return out, false, err
	}
	hashRaw, _ := json.Marshal(struct {
		Asset string
		Input rm.CreateInput
	}{asset, in})
	hash := au.Digest(string(hashRaw))
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM remediation_proposals WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			return out, false, rm.ErrConflict
		}
		out, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='expired',error_code='approval_expired' WHERE state='proposed' AND expires_at<=now()`); err != nil {
		return out, false, err
	}
	var pending int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM remediation_proposals WHERE state='proposed'`).Scan(&pending); err != nil {
		return out, false, err
	}
	if pending >= 100 {
		return out, false, rm.ErrUnavailable
	}
	drift, err := readDriftRecord(tx.QueryRow(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE id=$1 AND asset_id=$2`, in.DriftID, asset))
	if err != nil {
		return out, false, err
	}
	if drift.Status != "drift" || drift.Proposal == nil || drift.Truncated || drift.Normalizer != cd.NormalizerVersion || drift.ActualObservedAt.Before(time.Now().Add(-cd.DefaultMaxAge)) || drift.ActualObservedAt.After(time.Now().Add(30*time.Second)) {
		return out, false, rm.ErrConflict
	}
	var latest int64
	if err = tx.QueryRow(ctx, `SELECT id FROM configuration_drift_results WHERE asset_id=$1 AND baseline_revision=$2 ORDER BY actual_observed_at DESC,id DESC LIMIT 1`, asset, drift.BaselineRevision).Scan(&latest); err != nil {
		return out, false, err
	}
	if latest != drift.ID {
		return out, false, rm.ErrConflict
	}
	a, name, err := s.remediationIdentityTx(ctx, tx, asset, drift.Proposal.Interface)
	if err != nil {
		return out, false, err
	}
	baseline, err := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, asset))
	if err != nil {
		return out, false, err
	}
	if baseline.Revision != drift.BaselineRevision || baseline.AssetRevision != a.AssetRevision || baseline.Normalizer != cd.NormalizerVersion || drift.ActualTargetRevision != a.TargetRevision {
		return out, false, rm.ErrConflict
	}
	expected, err := s.driftSnapshotTx(ctx, tx, asset, baseline.SnapshotID)
	if err != nil {
		return out, false, err
	}
	actual, err := s.driftSnapshotTx(ctx, tx, asset, drift.ActualSnapshotID)
	if err != nil {
		return out, false, err
	}
	comparison := cd.Compare(baseline.ReadTemplateID, expected.Payload.Configuration, actual.Payload.Configuration)
	ep, e := cd.AccessPortState(baseline.ReadTemplateID, expected.Payload.Configuration, a.Interface)
	if e != nil {
		return out, false, rm.ErrPolicy
	}
	ap, e := cd.AccessPortState(baseline.ReadTemplateID, actual.Payload.Configuration, a.Interface)
	if e != nil {
		return out, false, rm.ErrPolicy
	}
	if comparison.Proposal == nil || comparison.Status != "drift" || comparison.ExpectedDigest != baseline.NormalizedDigest || comparison.ActualDigest != drift.ActualDigest || !ep.Safe || !ap.Safe {
		return out, false, rm.ErrConflict
	}
	a.DriftID = drift.ID
	a.BaselineRevision = baseline.Revision
	a.ExpectedSnapshotID = expected.ID
	a.ActualSnapshotID = actual.ID
	a.ExpectedDigest = comparison.ExpectedDigest
	a.ObservedDigest = comparison.ActualDigest
	a.CurrentVLAN = ap.VLAN
	a.TargetVLAN = ep.VLAN
	if a.Validate() != nil {
		return out, false, rm.ErrInvalid
	}
	if err = s.remediationPolicy.Authorize(actor.User.ID, a); err != nil {
		return out, false, err
	}
	id, err := access.NewID()
	if err != nil {
		return out, false, err
	}
	action, _ := json.Marshal(a)
	preview, _ := json.Marshal(struct {
		AssetName                            string
		ActualObservedAt, ExpectedObservedAt time.Time
		Warnings                             []string
	}{name, actual.ObservedAt, expected.ObservedAt, []string{"single_reviewed_physical_access_port", "running_configuration_only", "device_change_is_not_atomic"}})
	out, err = readRemediation(tx.QueryRow(ctx, `INSERT INTO remediation_proposals(id,asset_id,drift_id,action,digest,preview,created_by,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now()+interval '5 minutes') RETURNING `+remediationColumns, id, asset, drift.ID, action, rm.Fingerprint(a), preview, actor.User.ID, key, hash))
	if err != nil {
		return out, false, err
	}
	if err = automationAudit(ctx, tx, actor.User.ID, "remediation.proposal.created", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

// Revalidate all pinned inputs in a short transaction; never hold it over SSH.
func (s *Store) remediationRevalidateTx(ctx context.Context, tx pgx.Tx, p rm.Proposal, user string) (au.Target, cd.Baseline, error) {
	var target au.Target
	var baseline cd.Baseline
	if rm.Fingerprint(p.Action) != p.Digest || p.Action.Validate() != nil {
		return target, baseline, rm.ErrConflict
	}
	if !time.Now().Before(p.ExpiresAt) {
		return target, baseline, rm.ErrExpired
	}
	if !s.remediationPolicy.Enabled || s.remediationPolicy.Authorize(user, p.Action) != nil {
		return target, baseline, rm.ErrPolicy
	}
	a, _, err := s.remediationIdentityTx(ctx, tx, p.Action.AssetID, p.Action.Interface)
	if err != nil {
		return target, baseline, err
	}
	if a.AssetRevision != p.Action.AssetRevision || a.BindingID != p.Action.BindingID || a.BindingDigest != p.Action.BindingDigest || a.InterfaceID != p.Action.InterfaceID || a.TargetRevision != p.Action.TargetRevision || a.TargetDigest != p.Action.TargetDigest || a.PolicyDigest != p.Action.PolicyDigest || a.TemplateID != p.Action.TemplateID || a.TemplateDigest != p.Action.TemplateDigest {
		return target, baseline, rm.ErrConflict
	}
	baseline, err = readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, a.AssetID))
	if err != nil {
		return target, baseline, err
	}
	if baseline.Revision != p.Action.BaselineRevision || baseline.SnapshotID != p.Action.ExpectedSnapshotID || baseline.NormalizedDigest != p.Action.ExpectedDigest || baseline.AssetRevision != a.AssetRevision || baseline.Normalizer != cd.NormalizerVersion {
		return target, baseline, rm.ErrConflict
	}
	var latest cd.Record
	latest, err = readDriftRecord(tx.QueryRow(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE asset_id=$1 AND baseline_revision=$2 ORDER BY actual_observed_at DESC,id DESC LIMIT 1`, a.AssetID, baseline.Revision))
	if err != nil {
		return target, baseline, err
	}
	if latest.ActualDigest != p.Action.ObservedDigest || latest.Status != "drift" || latest.ActualTargetRevision != a.TargetRevision {
		return target, baseline, rm.ErrConflict
	}
	var raw []byte
	target.AssetID = a.AssetID
	target.Revision = a.TargetRevision
	if err = tx.QueryRow(ctx, `SELECT definition FROM automation_targets WHERE asset_id=$1`, a.AssetID).Scan(&raw); err != nil {
		return target, baseline, err
	}
	err = json.Unmarshal(raw, &target.TargetInput)
	return target, baseline, err
}

func (s *Store) ApproveRemediation(ctx context.Context, id string, in rm.ApproveInput) (rm.Proposal, bool, error) {
	p, replay, err := s.approveRemediation(ctx, id, in)
	if err != nil {
		s.recordRemediationDenial(ctx, id, err)
	}
	return p, replay, err
}
func (s *Store) approveRemediation(ctx context.Context, id string, in rm.ApproveInput) (rm.Proposal, bool, error) {
	var p rm.Proposal
	if !s.remediationReady || !s.remediationPolicy.Enabled {
		return p, false, rm.ErrUnavailable
	}
	if !au.Identifier(id) || in.Validate() != nil {
		return p, false, rm.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return p, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, false); err != nil {
		return p, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194004)`); err != nil {
		return p, false, err
	}
	p, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return p, false, err
	}
	if in.Digest != p.Digest {
		return p, false, rm.ErrConflict
	}
	if s.remediationPolicy.Authorize(actor.User.ID, p.Action) != nil {
		return p, false, rm.ErrPolicy
	}
	if p.ApprovedBy != "" {
		if p.ApprovedBy != actor.User.ID || p.ApprovedDigest != in.Digest {
			return p, false, rm.ErrConflict
		}
		return p, true, tx.Commit(ctx)
	}
	if p.State != "proposed" {
		return p, false, rm.ErrConflict
	}
	if s.remediationPolicy.RequireDistinctApprover && p.CreatedBy == actor.User.ID {
		return p, false, rm.ErrPolicy
	}
	target, _, err := s.remediationRevalidateTx(ctx, tx, p, actor.User.ID)
	if err != nil {
		state := "stale"
		if errors.Is(err, rm.ErrExpired) {
			state = "expired"
		}
		if _, e := tx.Exec(ctx, `UPDATE remediation_proposals SET state=$2,error_code='approval_validation_failed' WHERE id=$1`, id, state); e != nil {
			return p, false, e
		}
		if e := automationAudit(ctx, tx, actor.User.ID, "remediation.approval.validation", "denied", id, access.CurrentMeta(ctx)); e != nil {
			return p, false, e
		}
		if e := tx.Commit(ctx); e != nil {
			return p, false, e
		}
		return p, false, err
	}
	var count int
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM automation_jobs WHERE state IN ('queued','running','verifying')`).Scan(&count); err != nil {
		return p, false, err
	}
	if count >= 100 {
		return p, false, rm.ErrUnavailable
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_jobs WHERE asset_id=$1 AND state IN ('queued','running','verifying')) OR EXISTS(SELECT 1 FROM remediation_proposals WHERE asset_id=$1 AND state='unknown')`, p.Action.AssetID).Scan(&busy); err != nil {
		return p, false, err
	}
	if busy {
		return p, false, rm.ErrConflict
	}
	var available bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_workers WHERE available AND last_seen>now()-interval '20 seconds' AND templates ? $1 AND versions->>'restore_template_sha256'=$2)`, target.TemplateID, p.Action.TemplateDigest).Scan(&available); err != nil {
		return p, false, err
	}
	if !available {
		return p, false, rm.ErrUnavailable
	}
	jobID, err := access.NewID()
	if err != nil {
		return p, false, err
	}
	raw, _ := json.Marshal(target)
	if _, err = tx.Exec(ctx, `INSERT INTO automation_jobs(id,asset_id,target_revision,template_id,target,actor_id,session_hash,idempotency_key,request_hash,state,timeout_seconds,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',120,$10)`, jobID, target.AssetID, target.Revision, p.Action.TemplateID, raw, actor.User.ID, actor.SessionHash, "remediation-"+p.ID, p.Digest, in.Reason); err != nil {
		return p, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET approved_by=$2,approved_at=now(),approved_digest=digest,approval_request_id=$3,job_id=$4,state='queued' WHERE id=$1`, id, actor.User.ID, access.CurrentMeta(ctx).RequestID, jobID); err != nil {
		return p, false, err
	}
	for _, event := range []string{"approved", "queued"} {
		if err = automationEvent(ctx, tx, jobID, event); err != nil {
			return p, false, err
		}
		if err = automationAudit(ctx, tx, actor.User.ID, "remediation."+event, "success", id, access.CurrentMeta(ctx)); err != nil {
			return p, false, err
		}
	}
	p, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1`, id))
	if err != nil {
		return p, false, err
	}
	return p, false, tx.Commit(ctx)
}
func (s *Store) RejectRemediation(ctx context.Context, id string, in rm.RejectInput) (rm.Proposal, error) {
	var p rm.Proposal
	if !s.remediationReady {
		return p, rm.ErrUnavailable
	}
	if !au.Identifier(id) || in.Validate() != nil {
		return p, rm.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return p, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, false); err != nil {
		return p, err
	}
	p, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return p, err
	}
	if s.remediationPolicy.Authorize(actor.User.ID, p.Action) != nil {
		return p, rm.ErrPolicy
	}
	if p.State == "rejected" {
		return p, tx.Commit(ctx)
	}
	if p.State != "proposed" {
		return p, rm.ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='rejected' WHERE id=$1`, id); err != nil {
		return p, err
	}
	if err = automationAudit(ctx, tx, actor.User.ID, "remediation.approval.rejected", "success", id, access.CurrentMeta(ctx)); err != nil {
		return p, err
	}
	p.State = "rejected"
	return p, tx.Commit(ctx)
}

func (s *Store) recordRemediationDenial(ctx context.Context, resource string, err error) {
	if !errors.Is(err, rm.ErrPolicy) && !errors.Is(err, access.ErrForbidden) {
		return
	}
	actor, ok := access.Current(ctx)
	if !ok || !au.Identifier(resource) {
		return
	}
	bounded, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	meta := access.CurrentMeta(ctx)
	_, _ = s.pool.Exec(bounded, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'remediation.policy.validation','denied',$2,$3,$4)`, actor.User.ID, resource, meta.RequestID, meta.Peer)
}

// Reconcile an unknown outcome using a separately requested, successful B10
// read-only job. It cannot re-execute the write or claim its original success.
func (s *Store) ReconcileRemediation(ctx context.Context, id string, in rm.ReconcileInput) (rm.Proposal, error) {
	var p rm.Proposal
	if !s.remediationReady {
		return p, rm.ErrUnavailable
	}
	if !au.Identifier(id) || in.Validate() != nil {
		return p, rm.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return p, access.ErrForbidden
	}
	var jobID string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(job_id,'') FROM remediation_proposals WHERE id=$1`, id).Scan(&jobID); errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	} else if err != nil {
		return p, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, false); err != nil {
		return p, err
	}
	var deadline time.Time
	var unknownAt *time.Time
	var state string
	if err = tx.QueryRow(ctx, `SELECT state,deadline,finished_at FROM automation_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&state, &deadline, &unknownAt); err != nil {
		return p, err
	}
	p, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return p, err
	}
	if s.remediationPolicy.Authorize(actor.User.ID, p.Action) != nil {
		return p, rm.ErrPolicy
	}
	if p.State == "reconciled" && p.AfterSnapshotID == in.SnapshotID {
		return p, tx.Commit(ctx)
	}
	if p.State != "unknown" || state != "unknown" || unknownAt == nil || time.Now().Before(deadline.Add(35*time.Second)) {
		return p, rm.ErrConflict
	}
	identity, _, err := s.remediationIdentityTx(ctx, tx, p.Action.AssetID, p.Action.Interface)
	if err != nil {
		return p, err
	}
	if identity.BindingID != p.Action.BindingID || identity.InterfaceID != p.Action.InterfaceID {
		return p, rm.ErrConflict
	}
	snapshot, err := s.driftSnapshotTx(ctx, tx, p.Action.AssetID, in.SnapshotID)
	if err != nil {
		return p, err
	}
	if snapshot.JobID == jobID || snapshot.ID == p.Action.ActualSnapshotID || snapshot.ID == p.Action.ExpectedSnapshotID || !snapshot.ObservedAt.After(*unknownAt) || snapshot.TargetRevision != identity.TargetRevision || snapshot.AssetRevision != identity.AssetRevision || snapshot.TemplateID != "cisco_ios_read_config_v1" || snapshot.ObservedAt.Before(deadline.Add(35*time.Second)) || snapshot.ObservedAt.Before(time.Now().Add(-cd.DefaultMaxAge)) || snapshot.ObservedAt.After(time.Now().Add(30*time.Second)) {
		return p, rm.ErrConflict
	}
	var readOnly bool
	if err = tx.QueryRow(ctx, `SELECT template_id='cisco_ios_read_config_v1' AND NOT EXISTS(SELECT 1 FROM remediation_proposals WHERE job_id=$1) FROM automation_jobs WHERE id=$1`, snapshot.JobID).Scan(&readOnly); err != nil {
		return p, err
	}
	if !readOnly {
		return p, rm.ErrConflict
	}
	baseline, err := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, p.Action.AssetID))
	if err != nil {
		return p, err
	}
	if baseline.AssetRevision != identity.AssetRevision || baseline.ReadTemplateID != snapshot.TemplateID || baseline.Normalizer != cd.NormalizerVersion {
		return p, rm.ErrConflict
	}
	record, err := s.compareConfigurationTx(ctx, tx, baseline, snapshot)
	if err != nil {
		return p, err
	}
	if record.Status == "unknown" {
		return p, rm.ErrConflict
	}
	verification := rm.Verification{Status: record.Status, Comparison: record.Comparison, VerifiedAt: time.Now().UTC(), DriftID: record.ID}
	raw, _ := json.Marshal(verification)
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='reconciled',error_code='original_execution_unknown',after_snapshot_id=$2,verification=$3 WHERE id=$1`, id, in.SnapshotID, raw); err != nil {
		return p, err
	}
	if err = automationAudit(ctx, tx, actor.User.ID, "remediation.unknown.reconciled", record.Status, id, access.CurrentMeta(ctx)); err != nil {
		return p, err
	}
	p.State = "reconciled"
	p.ErrorCode = "original_execution_unknown"
	p.AfterSnapshotID = in.SnapshotID
	p.Verification = &verification
	return p, tx.Commit(ctx)
}
