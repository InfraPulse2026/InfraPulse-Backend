package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"github.com/jackc/pgx/v5"
)

const remediationColumns = `id,asset_id,revision,drift_result_id,baseline_revision,actual_snapshot_id,policy_revision,action,interface_name,expected_vlan,actual_vlan,state,error_code,requester_id,COALESCE(approver_id,''),reason,approval_reason,COALESCE(automation_job_id,''),COALESCE(verification_drift_result_id,0),created_at,approved_at,finished_at`

func readRemediation(row interface{ Scan(...any) error }) (cd.Remediation, error) {
	var v cd.Remediation
	err := row.Scan(&v.ID, &v.AssetID, &v.Revision, &v.DriftResultID, &v.BaselineRevision, &v.ActualSnapshotID, &v.PolicyRevision, &v.Action, &v.Interface, &v.ExpectedVLAN, &v.ActualVLAN, &v.State, &v.ErrorCode, &v.RequesterID, &v.ApproverID, &v.Reason, &v.ApprovalReason, &v.AutomationJobID, &v.VerificationID, &v.CreatedAt, &v.ApprovedAt, &v.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func readRemediationPolicy(row interface{ Scan(...any) error }) (cd.Policy, error) {
	var v cd.Policy
	var vlans, interfaces []byte
	err := row.Scan(&v.AssetID, &v.Revision, &v.Enabled, &vlans, &interfaces, &v.MaxEvidenceAgeSeconds, &v.ActorID, &v.Reason, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if json.Unmarshal(vlans, &v.AllowedVLANs) != nil || json.Unmarshal(interfaces, &v.ProtectedInterfaces) != nil {
		return v, cd.ErrConflict
	}
	v.ExpectedRevision = v.Revision
	return v, nil
}

func (s *Store) ConfigurationRemediationPolicy(ctx context.Context, asset string) (cd.Policy, error) {
	if !au.Identifier(asset) {
		return cd.Policy{}, cd.ErrInvalid
	}
	return readRemediationPolicy(s.pool.QueryRow(ctx, `SELECT asset_id,revision,enabled,allowed_vlans,protected_interfaces,max_evidence_age_seconds,actor_id,reason,updated_at FROM configuration_remediation_policies WHERE asset_id=$1`, asset))
}

func (s *Store) PutConfigurationRemediationPolicy(ctx context.Context, asset string, in cd.PolicyInput) (cd.Policy, error) {
	var out cd.Policy
	if !s.automationEnabled {
		return out, au.ErrUnavailable
	}
	if !au.Identifier(asset) || in.Validate() != nil {
		return out, cd.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, true); err != nil {
		return out, err
	}
	var lifecycle, scope string
	if err = tx.QueryRow(ctx, `SELECT lifecycle_status,scope FROM assets WHERE id=$1 FOR SHARE`, asset).Scan(&lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	if lifecycle != "observed" || scope != "" {
		return out, cd.ErrConflict
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM configuration_remediation_policies WHERE asset_id=$1 FOR UPDATE`, asset).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if current != in.ExpectedRevision || current == int64(^uint64(0)>>1) {
		return out, cd.ErrConflict
	}
	vlans, _ := json.Marshal(in.AllowedVLANs)
	protected, _ := json.Marshal(in.ProtectedInterfaces)
	out = cd.Policy{AssetID: asset, Revision: current + 1, PolicyInput: in, ActorID: p.User.ID}
	err = tx.QueryRow(ctx, `INSERT INTO configuration_remediation_policies(asset_id,revision,enabled,allowed_vlans,protected_interfaces,max_evidence_age_seconds,actor_id,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(asset_id) DO UPDATE SET revision=excluded.revision,enabled=excluded.enabled,allowed_vlans=excluded.allowed_vlans,protected_interfaces=excluded.protected_interfaces,max_evidence_age_seconds=excluded.max_evidence_age_seconds,actor_id=excluded.actor_id,reason=excluded.reason,updated_at=now() RETURNING updated_at`, asset, out.Revision, in.Enabled, vlans, protected, in.MaxEvidenceAgeSeconds, p.User.ID, in.Reason).Scan(&out.UpdatedAt)
	if err != nil {
		return out, err
	}
	out.ExpectedRevision = out.Revision
	if _, err = tx.Exec(ctx, `INSERT INTO configuration_remediation_policy_history(asset_id,revision,enabled,allowed_vlans,protected_interfaces,max_evidence_age_seconds,actor_id,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, asset, out.Revision, in.Enabled, vlans, protected, in.MaxEvidenceAgeSeconds, p.User.ID, in.Reason, out.UpdatedAt); err != nil {
		return out, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "configuration.remediation.policy.update", "success", asset, access.CurrentMeta(ctx)); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func policyAllows(policy cd.Policy, proposal *cd.Proposal) bool {
	if !policy.Enabled || proposal == nil || !proposal.ExecutionAvailable || proposal.Action != "restore_access_vlan" {
		return false
	}
	allowed := false
	for _, vlan := range policy.AllowedVLANs {
		allowed = allowed || vlan == proposal.ExpectedVLAN
	}
	for _, name := range policy.ProtectedInterfaces {
		if name == proposal.Interface {
			return false
		}
	}
	return allowed
}

func (s *Store) remediationEvidenceTx(ctx context.Context, tx pgx.Tx, asset string, driftID, policyRevision int64) (cd.Record, cd.Policy, au.Target, error) {
	var emptyRecord cd.Record
	var emptyPolicy cd.Policy
	var target au.Target
	policy, err := readRemediationPolicy(tx.QueryRow(ctx, `SELECT asset_id,revision,enabled,allowed_vlans,protected_interfaces,max_evidence_age_seconds,actor_id,reason,updated_at FROM configuration_remediation_policies WHERE asset_id=$1 FOR SHARE`, asset))
	if err != nil {
		return emptyRecord, emptyPolicy, target, err
	}
	if policy.Revision != policyRevision {
		return emptyRecord, policy, target, cd.ErrConflict
	}
	var assetRevision int64
	var lifecycle, scope string
	if err = tx.QueryRow(ctx, `SELECT revision,lifecycle_status,scope FROM assets WHERE id=$1 FOR SHARE`, asset).Scan(&assetRevision, &lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return emptyRecord, policy, target, ErrNotFound
	} else if err != nil {
		return emptyRecord, policy, target, err
	}
	baseline, err := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, asset))
	if err != nil {
		return emptyRecord, policy, target, err
	}
	record, err := readDriftRecord(tx.QueryRow(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE asset_id=$1 AND id=$2`, asset, driftID))
	if err != nil {
		return emptyRecord, policy, target, err
	}
	var latestID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM configuration_drift_results WHERE asset_id=$1 AND baseline_revision=$2 ORDER BY actual_observed_at DESC,id DESC LIMIT 1`, asset, baseline.Revision).Scan(&latestID); err != nil {
		return emptyRecord, policy, target, err
	}
	var definition []byte
	if err = tx.QueryRow(ctx, `SELECT asset_id,revision,definition,updated_at FROM automation_targets WHERE asset_id=$1 FOR SHARE`, asset).Scan(&target.AssetID, &target.Revision, &definition, &target.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return emptyRecord, policy, target, ErrNotFound
	} else if err != nil {
		return emptyRecord, policy, target, err
	}
	if json.Unmarshal(definition, &target.TargetInput) != nil {
		return emptyRecord, policy, target, cd.ErrConflict
	}
	fresh := record.ActualObservedAt.Before(time.Now().Add(30*time.Second)) && record.ActualObservedAt.After(time.Now().Add(-time.Duration(policy.MaxEvidenceAgeSeconds)*time.Second))
	supported := target.TemplateID == "cisco_ios_read_config_v1" || target.TemplateID == "arista_eos_read_config_v1"
	if lifecycle != "observed" || scope != "" || assetRevision != baseline.AssetRevision || latestID != record.ID || record.BaselineRevision != baseline.Revision || record.Status != "drift" || record.Proposal == nil || !fresh || !target.Enabled || record.ActualTargetRevision != target.Revision || target.AssetRevision != assetRevision || target.TemplateID != baseline.ReadTemplateID || !supported || !policyAllows(policy, record.Proposal) {
		return record, policy, target, cd.ErrConflict
	}
	return record, policy, target, nil
}

func (s *Store) CreateConfigurationRemediation(ctx context.Context, asset string, in cd.RemediationInput, key string) (cd.Remediation, bool, error) {
	var out cd.Remediation
	if !s.automationEnabled {
		return out, false, au.ErrUnavailable
	}
	if !au.Identifier(asset) || in.Validate() != nil || commands.ValidateKey(key) != nil {
		return out, false, cd.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, false); err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,73194006))`, p.User.ID+":"+key); err != nil {
		return out, false, err
	}
	raw, _ := json.Marshal(struct {
		Asset string
		Input cd.RemediationInput
	}{asset, in})
	hash := au.Digest(string(raw))
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM configuration_remediations WHERE requester_id=$1 AND request_idempotency_key=$2`, p.User.ID, key).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			return out, false, cd.ErrConflict
		}
		out, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE requester_id=$1 AND request_idempotency_key=$2`, p.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,73194008))`, asset); err != nil {
		return out, false, err
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configuration_remediations WHERE asset_id=$1 AND state IN ('pending_approval','queued','running','cancel_requested'))`, asset).Scan(&busy); err != nil {
		return out, false, err
	}
	if busy {
		return out, false, cd.ErrConflict
	}
	record, _, _, err := s.remediationEvidenceTx(ctx, tx, asset, in.DriftResultID, in.PolicyRevision)
	if err != nil {
		return out, false, err
	}
	id, err := access.NewID()
	if err != nil {
		return out, false, err
	}
	pv := record.Proposal
	out, err = readRemediation(tx.QueryRow(ctx, `INSERT INTO configuration_remediations(id,asset_id,drift_result_id,baseline_revision,actual_snapshot_id,policy_revision,action,interface_name,expected_vlan,actual_vlan,state,requester_id,reason,request_idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending_approval',$11,$12,$13,$14) RETURNING `+remediationColumns, id, asset, record.ID, record.BaselineRevision, record.ActualSnapshotID, in.PolicyRevision, pv.Action, pv.Interface, pv.ExpectedVLAN, pv.ActualVLAN, p.User.ID, in.Reason, key, hash))
	if err != nil {
		return out, false, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "configuration.remediation.request", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

func (s *Store) ApproveConfigurationRemediation(ctx context.Context, id string, in cd.ApprovalInput, key string) (cd.Remediation, bool, error) {
	var out cd.Remediation
	if !s.automationEnabled {
		return out, false, au.ErrUnavailable
	}
	if !au.Identifier(id) || in.Validate() != nil || commands.ValidateKey(key) != nil {
		return out, false, cd.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, true); err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,73194007))`, p.User.ID+":"+key); err != nil {
		return out, false, err
	}
	raw, _ := json.Marshal(struct {
		ID    string
		Input cd.ApprovalInput
	}{id, in})
	hash := au.Digest(string(raw))
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT approval_hash FROM configuration_remediations WHERE approver_id=$1 AND approval_idempotency_key=$2`, p.User.ID, key).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			return out, false, cd.ErrConflict
		}
		out, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE approver_id=$1 AND approval_idempotency_key=$2`, p.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	out, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return out, false, err
	}
	if out.State != "pending_approval" || out.Revision != in.ExpectedRevision {
		return out, false, cd.ErrConflict
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194004)`); err != nil {
		return out, false, err
	}
	record, _, target, err := s.remediationEvidenceTx(ctx, tx, out.AssetID, out.DriftResultID, out.PolicyRevision)
	if err != nil || record.Proposal == nil || record.Proposal.Interface != out.Interface || record.Proposal.ExpectedVLAN != out.ExpectedVLAN || record.Proposal.ActualVLAN != out.ActualVLAN {
		if err == nil {
			err = cd.ErrConflict
		}
		return out, false, err
	}
	var outstanding int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM automation_jobs WHERE state IN ('queued','running')`).Scan(&outstanding); err != nil {
		return out, false, err
	}
	var busy, available bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_jobs WHERE asset_id=$1 AND state IN ('queued','running'))`, out.AssetID).Scan(&busy); err != nil || busy || outstanding >= 100 {
		if err == nil {
			if outstanding >= 100 {
				err = au.ErrUnavailable
			} else {
				err = cd.ErrConflict
			}
		}
		return out, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_workers WHERE available AND last_seen>now()-interval '20 seconds' AND templates ? $1)`, target.TemplateID).Scan(&available); err != nil {
		return out, false, err
	}
	if !available {
		return out, false, au.ErrUnavailable
	}
	jobID, err := access.NewID()
	if err != nil {
		return out, false, err
	}
	target.Operation = &au.Operation{Type: out.Action, RemediationID: out.ID, Interface: out.Interface, VLAN: out.ExpectedVLAN}
	if target.Operation.Validate() != nil {
		return out, false, cd.ErrConflict
	}
	targetJSON, _ := json.Marshal(target)
	jobKey := "remediation:" + out.ID
	jobHash := au.Digest(jobKey)
	if _, err = tx.Exec(ctx, `INSERT INTO automation_jobs(id,asset_id,target_revision,template_id,target,actor_id,session_hash,idempotency_key,request_hash,state,timeout_seconds,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$11)`, jobID, out.AssetID, target.Revision, target.TemplateID, targetJSON, p.User.ID, p.SessionHash, jobKey, jobHash, in.TimeoutSeconds, "approved configuration remediation: "+in.Reason); err != nil {
		return out, false, err
	}
	if err = automationEvent(ctx, tx, jobID, "queued"); err != nil {
		return out, false, err
	}
	out, err = readRemediation(tx.QueryRow(ctx, `UPDATE configuration_remediations SET revision=revision+1,state='queued',approver_id=$2,approval_reason=$3,approval_idempotency_key=$4,approval_hash=$5,automation_job_id=$6,approved_at=now() WHERE id=$1 RETURNING `+remediationColumns, id, p.User.ID, in.Reason, key, hash, jobID))
	if err != nil {
		return out, false, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "configuration.remediation.approve", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

func (s *Store) CancelConfigurationRemediation(ctx context.Context, id string, in cd.CancelInput) (cd.Remediation, error) {
	var out cd.Remediation
	if !au.Identifier(id) || in.Validate() != nil {
		return out, cd.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	role, err := automationActorTx(ctx, tx, p, false)
	if err != nil {
		return out, err
	}
	out, err = readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return out, err
	}
	if role != "admin" && out.RequesterID != p.User.ID {
		return out, access.ErrForbidden
	}
	if out.Revision != in.ExpectedRevision || out.State != "pending_approval" && out.State != "queued" && out.State != "running" {
		return out, cd.ErrConflict
	}
	if out.AutomationJobID != "" {
		if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET cancel_requested=true,state=CASE WHEN state='queued' THEN 'cancelled' ELSE state END,finished_at=CASE WHEN state='queued' THEN now() ELSE finished_at END WHERE id=$1 AND state IN ('queued','running')`, out.AutomationJobID); err != nil {
			return out, err
		}
		if err = automationEvent(ctx, tx, out.AutomationJobID, "cancel_requested"); err != nil {
			return out, err
		}
	}
	if out.State == "running" {
		out, err = readRemediation(tx.QueryRow(ctx, `UPDATE configuration_remediations SET revision=revision+1,state='cancel_requested' WHERE id=$1 RETURNING `+remediationColumns, id))
		if err != nil {
			return out, err
		}
	} else {
		out, err = readRemediation(tx.QueryRow(ctx, `UPDATE configuration_remediations SET revision=revision+1,state='cancelled',error_code='cancelled',finished_at=now() WHERE id=$1 RETURNING `+remediationColumns, id))
		if err != nil {
			return out, err
		}
	}
	if err = automationAudit(ctx, tx, p.User.ID, "configuration.remediation.cancel", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) ConfigurationRemediation(ctx context.Context, id string) (cd.Remediation, error) {
	if !au.Identifier(id) {
		return cd.Remediation{}, cd.ErrInvalid
	}
	return readRemediation(s.pool.QueryRow(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE id=$1`, id))
}

func (s *Store) ConfigurationRemediations(ctx context.Context, asset string, limit, offset int) ([]cd.Remediation, error) {
	if !au.Identifier(asset) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, cd.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+remediationColumns+` FROM configuration_remediations WHERE asset_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, asset, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cd.Remediation{}
	for rows.Next() {
		v, e := readRemediation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) remediationRunValidTx(ctx context.Context, q pgx.Tx, jobID string) (bool, error) {
	var id, asset string
	var driftID, policyRevision int64
	err := q.QueryRow(ctx, `SELECT id,asset_id,drift_result_id,policy_revision FROM configuration_remediations WHERE automation_job_id=$1 AND state IN ('queued','running')`, jobID).Scan(&id, &asset, &driftID, &policyRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		var linked bool
		if e := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configuration_remediations WHERE automation_job_id=$1)`, jobID).Scan(&linked); e != nil {
			return false, e
		}
		return !linked, nil
	}
	if err != nil {
		return false, err
	}
	record, _, target, err := s.remediationEvidenceTx(ctx, q, asset, driftID, policyRevision)
	if errors.Is(err, cd.ErrConflict) || errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var raw []byte
	if err = q.QueryRow(ctx, `SELECT target FROM automation_jobs WHERE id=$1`, jobID).Scan(&raw); err != nil {
		return false, err
	}
	var pinned au.Target
	if json.Unmarshal(raw, &pinned) != nil || pinned.Operation == nil || pinned.Operation.Validate() != nil || pinned.Operation.RemediationID != id || pinned.Operation.Interface != record.Proposal.Interface || pinned.Operation.VLAN != record.Proposal.ExpectedVLAN || pinned.Revision != target.Revision {
		return false, nil
	}
	return true, nil
}

func (s *Store) finishConfigurationRemediationTx(ctx context.Context, tx pgx.Tx, jobID, state, errorCode string, snapshotID *string) error {
	var id, asset string
	err := tx.QueryRow(ctx, `SELECT id,asset_id FROM configuration_remediations WHERE automation_job_id=$1 FOR UPDATE`, jobID).Scan(&id, &asset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	finalState := "failed"
	verificationID := int64(0)
	code := errorCode
	if state == "cancelled" || state == "timed_out" {
		finalState = "cancelled"
	}
	if state == "succeeded" && snapshotID != nil {
		var status string
		err = tx.QueryRow(ctx, `SELECT id,status FROM configuration_drift_results WHERE asset_id=$1 AND actual_snapshot_id=$2 ORDER BY id DESC LIMIT 1`, asset, *snapshotID).Scan(&verificationID, &status)
		if err == nil && status == "compliant" {
			finalState = "verified"
			code = ""
		} else {
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			code = "verification_failed"
		}
	}
	_, err = tx.Exec(ctx, `UPDATE configuration_remediations SET revision=revision+1,state=$2,error_code=$3,verification_drift_result_id=NULLIF($4,0),finished_at=now() WHERE id=$1`, id, finalState, code, verificationID)
	return err
}
