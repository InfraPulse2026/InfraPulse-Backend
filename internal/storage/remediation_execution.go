package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
	"github.com/jackc/pgx/v5"
)

func (s *Store) claimRemediationTx(ctx context.Context, tx pgx.Tx, j *au.Job, worker string) (bool, error) {
	if !s.remediationReady {
		return true, nil
	}
	p, err := readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE job_id=$1 FOR UPDATE`, j.ID))
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if p.State != "queued" || p.ApprovedDigest != p.Digest || p.ApprovedBy != j.ActorID {
		return false, rm.ErrConflict
	}
	_, _, err = s.remediationRevalidateTx(ctx, tx, p, j.ActorID)
	var available bool
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT available AND last_seen>now()-interval '20 seconds' AND versions->>'restore_template_sha256'=$2 FROM automation_workers WHERE id=$1`, worker, p.Action.TemplateDigest).Scan(&available)
		if err == nil && !available {
			err = rm.ErrConflict
		}
	}
	if err == nil && time.Since(j.CreatedAt) > time.Minute {
		err = rm.ErrExpired
	}
	if err != nil {
		if _, e := tx.Exec(ctx, `UPDATE automation_jobs SET state='stale',error_code='approval_validation_failed',finished_at=now(),claim_token=NULL WHERE id=$1`, j.ID); e != nil {
			return false, e
		}
		if _, e := tx.Exec(ctx, `UPDATE remediation_proposals SET state='stale',error_code='approval_validation_failed' WHERE id=$1`, p.ID); e != nil {
			return false, e
		}
		if e := automationEvent(ctx, tx, j.ID, "approval_validation_failed"); e != nil {
			return false, e
		}
		return false, automationAudit(ctx, tx, j.ActorID, "remediation.start.validation", "denied", p.ID, access.Meta{})
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='running' WHERE id=$1`, p.ID); err != nil {
		return false, err
	}
	j.ProposalID = p.ID
	j.ActionDigest = p.Digest
	j.Restore = &au.RestoreAccessVLAN{TemplateID: p.Action.TemplateID, TemplateDigest: p.Action.TemplateDigest, Interface: p.Action.Interface, CurrentVLAN: p.Action.CurrentVLAN, TargetVLAN: p.Action.TargetVLAN}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.job.started", "success", p.ID, access.Meta{}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) recoverRemediationTx(ctx context.Context, tx pgx.Tx) error {
	if err := s.expireQueuedRemediationsTx(ctx, tx); err != nil {
		return err
	}
	// No claim ownership transfer/requeue for an operation that may have written.
	rows, err := tx.Query(ctx, `UPDATE automation_jobs j SET state=CASE WHEN EXISTS(SELECT 1 FROM remediation_proposals p WHERE p.job_id=j.id) THEN 'unknown' ELSE 'failed' END,error_code='worker_lost',finished_at=now(),claim_token=NULL WHERE j.state IN ('running','verifying') AND deadline<now()-interval '30 seconds' RETURNING j.id,j.state,j.actor_id`)
	if err != nil {
		return err
	}
	type lost struct{ id, state, actor string }
	items := []lost{}
	for rows.Next() {
		var v lost
		if err = rows.Scan(&v.id, &v.state, &v.actor); err != nil {
			break
		}
		items = append(items, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		if err = automationEvent(ctx, tx, v.id, "worker_lost"); err != nil {
			return err
		}
		if v.state == "unknown" {
			var id string
			if err = tx.QueryRow(ctx, `UPDATE remediation_proposals SET state='unknown',error_code='worker_lost' WHERE job_id=$1 RETURNING id`, v.id).Scan(&id); err != nil {
				return err
			}
			if err = automationAudit(ctx, tx, v.actor, "remediation.job.unknown", "worker_lost", id, access.Meta{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) expireQueuedRemediationsTx(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `UPDATE automation_jobs j SET state='stale',error_code='approval_validation_failed',finished_at=now() FROM remediation_proposals p WHERE p.job_id=j.id AND j.state='queued' AND (p.expires_at<=now() OR j.created_at<now()-interval '1 minute') RETURNING j.id,j.actor_id,p.id`)
	if err != nil {
		return err
	}
	type expired struct{ job, actor, proposal string }
	items := []expired{}
	for rows.Next() {
		var v expired
		if err = rows.Scan(&v.job, &v.actor, &v.proposal); err != nil {
			break
		}
		items = append(items, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='stale',error_code='approval_validation_failed' WHERE id=$1 AND state='queued'`, v.proposal); err != nil {
			return err
		}
		if err = automationEvent(ctx, tx, v.job, "queue_expired"); err != nil {
			return err
		}
		if err = automationAudit(ctx, tx, v.actor, "remediation.queue.expired", "denied", v.proposal, access.Meta{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) remediationJobTx(ctx context.Context, tx pgx.Tx, j au.Job) (rm.Proposal, error) {
	// Uniform order across claim, cancellation, recovery and phase completion:
	// job first, proposal second. Never invert it while holding row locks.
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM automation_jobs WHERE id=$1 FOR UPDATE`, j.ID).Scan(&locked); err != nil {
		return rm.Proposal{}, err
	}
	p, err := readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE job_id=$1 FOR UPDATE`, j.ID))
	if err != nil {
		return p, err
	}
	if p.ID != j.ProposalID || p.Digest != j.ActionDigest || p.ApprovedBy != j.ActorID || p.ApprovedDigest != p.Digest {
		return p, rm.ErrConflict
	}
	var valid bool
	if err = tx.QueryRow(ctx, remediationRunValid, j.ID, j.ClaimToken).Scan(&valid); err != nil {
		return p, err
	}
	if !valid {
		return p, rm.ErrConflict
	}
	return p, nil
}

const remediationRunValid = `SELECT EXISTS(SELECT 1 FROM automation_jobs j JOIN assets a ON a.id=j.asset_id JOIN automation_targets t ON t.asset_id=j.asset_id JOIN access_users u ON u.id=j.actor_id JOIN access_sessions ses ON ses.user_id=u.id AND ses.token_hash=j.session_hash WHERE j.id=$1 AND j.state IN ('running','verifying') AND j.claim_token=$2 AND NOT j.cancel_requested AND j.deadline>now() AND t.revision=j.target_revision AND (t.definition->>'enabled')::boolean AND a.revision=(t.definition->>'asset_revision')::bigint AND a.lifecycle_status='observed' AND a.scope='' AND NOT u.disabled AND u.role IN ('admin','operator') AND ses.expires_at>now())`

func (s *Store) remediationSnapshotTx(ctx context.Context, tx pgx.Tx, j au.Job, phase string, r au.Result) (driftSnapshot, error) {
	var snap driftSnapshot
	if r.Validate() != nil {
		return snap, au.ErrInvalid
	}
	id, err := access.NewID()
	if err != nil {
		return snap, err
	}
	payload, _ := json.Marshal(r)
	cipher, err := s.automationVault.Seal("automation-snapshot:"+id, string(payload))
	clear(payload)
	if err != nil {
		return snap, err
	}
	snap = driftSnapshot{Snapshot: au.Snapshot{ID: id, AssetID: j.AssetID, JobID: j.ID, TemplateID: j.Target.TemplateID, Digest: au.Digest(r.Configuration), Bytes: len(r.Configuration)}, TargetRevision: j.TargetRevision, AssetRevision: j.Target.AssetRevision, Payload: r}
	err = tx.QueryRow(ctx, `INSERT INTO automation_snapshots(id,asset_id,job_id,template_id,digest,bytes,ciphertext,phase) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING observed_at`, id, j.AssetID, j.ID, snap.TemplateID, snap.Digest, snap.Bytes, cipher, phase).Scan(&snap.ObservedAt)
	return snap, err
}

// ExecuteRemediation is a phase coordinator inside the EXISTING worker. Each
// device interaction goes through the existing bounded Executor, outside DB TX.
func (s *Store) ExecuteRemediation(ctx context.Context, executor au.Executor, j au.Job) (string, au.Result) {
	if !s.remediationReady || j.Restore == nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	// Use only parameters derived from the persisted approval, even if a caller
	// accidentally mutates an in-process claimed Job value.
	pinned, pinErr := readRemediation(s.pool.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE job_id=$1`, j.ID))
	if pinErr != nil || pinned.ID != j.ProposalID || pinned.Digest != j.ActionDigest || j.AssetID != pinned.Action.AssetID {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	targetRaw, _ := json.Marshal(j.Target.TargetInput)
	if au.Digest(string(targetRaw)) != pinned.Action.TargetDigest || j.TargetRevision != pinned.Action.TargetRevision || j.TemplateID != pinned.Action.TemplateID {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	j.Restore = &au.RestoreAccessVLAN{TemplateID: pinned.Action.TemplateID, TemplateDigest: pinned.Action.TemplateDigest, Interface: pinned.Action.Interface, CurrentVLAN: pinned.Action.CurrentVLAN, TargetVLAN: pinned.Action.TargetVLAN}
	readJob := j
	readJob.Restore = nil
	readJob.TemplateID = j.Target.TemplateID
	gatherCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	before := executor.Execute(gatherCtx, readJob)
	cancel()
	if before.Validate() != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	defer tx.Rollback(context.Background())
	p, err := s.remediationJobTx(ctx, tx, j)
	if err != nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	_, baseline, err := s.remediationRevalidateTx(ctx, tx, p, j.ActorID)
	if err != nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	expected, err := s.driftSnapshotTx(ctx, tx, j.AssetID, p.Action.ExpectedSnapshotID)
	if err != nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	snap, err := s.remediationSnapshotTx(ctx, tx, j, "preflight", before)
	if err != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET before_snapshot_id=$2 WHERE id=$1`, p.ID, snap.ID); err != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	comparison := cd.Compare(baseline.ReadTemplateID, expected.Payload.Configuration, before.Configuration)
	state := ""
	if comparison.Status == "compliant" {
		state = "no_action"
	} else if comparison.ActualDigest != p.Action.ObservedDigest || comparison.ExpectedDigest != p.Action.ExpectedDigest || comparison.Proposal == nil {
		state = "stale"
	}
	port, e := cd.AccessPortState(baseline.ReadTemplateID, before.Configuration, p.Action.Interface)
	if state == "" && (e != nil || !port.Safe || port.VLAN != p.Action.CurrentVLAN) {
		state = "stale"
	}
	outcome := "passed"
	if state != "" {
		outcome = state
	}
	if err = automationEvent(ctx, tx, j.ID, "preflight_"+outcome); err != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.preflight", outcome, p.ID, access.Meta{}); err != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	if err = tx.Commit(ctx); err != nil {
		return "failed", au.Result{ErrorCode: "preflight_unavailable"}
	}
	if state != "" {
		return state, before
	}
	// Last authorization/ownership check immediately before the write dispatch.
	valid, err := s.AutomationRunning(ctx, j)
	if err != nil || !valid {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	if err = s.recordRemediationWriteDispatch(ctx, j); err != nil {
		return "stale", au.Result{ErrorCode: "approval_validation_failed"}
	}
	writeResult := executor.Execute(ctx, j)
	// A guard failure is definitely before ios_config. Other errors may conceal
	// a completed change, so never label them a harmless failure or retry.
	if writeResult.ErrorCode == "precondition_failed" {
		return "stale", au.Result{ErrorCode: "preflight_changed"}
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer verifyCancel()
	if err = s.beginRemediationVerification(verifyCtx, j); err != nil {
		return "unknown", au.Result{ErrorCode: "execution_uncertain"}
	}
	after := executor.Execute(verifyCtx, readJob)
	if after.Validate() != nil {
		return "unknown", au.Result{ErrorCode: "verification_unavailable"}
	}
	if writeResult.ErrorCode != "" {
		after.ErrorCode = "execution_uncertain"
		return "unknown", after
	}
	verified := cd.Compare(baseline.ReadTemplateID, expected.Payload.Configuration, after.Configuration)
	afterPort, e := cd.AccessPortState(baseline.ReadTemplateID, after.Configuration, p.Action.Interface)
	if e != nil || !afterPort.Safe || afterPort.VLAN != p.Action.TargetVLAN || verified.Status != "compliant" || verified.ExpectedDigest != p.Action.ExpectedDigest {
		return "verification_failed", after
	}
	return "succeeded", after
}
func (s *Store) recordRemediationWriteDispatch(ctx context.Context, j au.Job) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	p, err := s.remediationJobTx(ctx, tx, j)
	if err != nil {
		return err
	}
	if _, _, err = s.remediationRevalidateTx(ctx, tx, p, j.ActorID); err != nil {
		return err
	}
	if err = automationEvent(ctx, tx, j.ID, "write_dispatch"); err != nil {
		return err
	}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.execution.started", "dispatching", p.ID, access.Meta{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) beginRemediationVerification(ctx context.Context, j au.Job) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	p, err := s.remediationJobTx(ctx, tx, j)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET state='verifying' WHERE id=$1`, j.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state='verifying' WHERE id=$1`, p.ID); err != nil {
		return err
	}
	if err = automationEvent(ctx, tx, j.ID, "verifying"); err != nil {
		return err
	}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.verification.started", "success", p.ID, access.Meta{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) finishRemediation(ctx context.Context, j au.Job, state string, result au.Result) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var current, token, asset, template, actor string
	var targetRaw []byte
	var cancelled bool
	var deadline time.Time
	if err = tx.QueryRow(ctx, `SELECT state,COALESCE(claim_token,''),asset_id,template_id,actor_id,cancel_requested,deadline,target FROM automation_jobs WHERE id=$1 FOR UPDATE`, j.ID).Scan(&current, &token, &asset, &template, &actor, &cancelled, &deadline, &targetRaw); err != nil {
		return err
	}
	p, err := readRemediation(tx.QueryRow(ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE job_id=$1 FOR UPDATE`, j.ID))
	if err != nil {
		return err
	}
	// Duplicate/late callbacks never change terminal outcome or execute again.
	if current != "running" && current != "verifying" {
		return au.ErrConflict
	}
	if token != j.ClaimToken || asset != j.AssetID || template != j.TemplateID || actor != j.ActorID || p.ID != j.ProposalID || p.Digest != j.ActionDigest {
		return au.ErrConflict
	}
	if err = json.Unmarshal(targetRaw, &j.Target); err != nil {
		return err
	}
	switch state {
	case "succeeded", "verification_failed", "unknown", "stale", "no_action", "failed", "cancelled":
	default:
		return au.ErrInvalid
	}
	code := result.ErrorCode
	if cancelled || !time.Now().Before(deadline) {
		state = "unknown"
		code = "execution_uncertain"
	}
	var valid bool
	if err = tx.QueryRow(ctx, remediationRunValid, j.ID, j.ClaimToken).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		state = "unknown"
		code = "execution_uncertain"
	}
	var afterID *string
	var verification *rm.Verification
	// Error codes are not part of gathered configuration evidence.
	evidence := result
	evidence.ErrorCode = ""
	if evidence.Validate() == nil && (current == "verifying" || state == "no_action" || state == "stale") {
		snap, e := s.remediationSnapshotTx(ctx, tx, j, "result", evidence)
		if e != nil {
			return e
		}
		afterID = &snap.ID
		baseline, e := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v WHERE asset_id=$1 AND revision=$2`, asset, p.Action.BaselineRevision))
		if e != nil {
			return e
		}
		record, e := s.compareConfigurationTx(ctx, tx, baseline, snap)
		if e != nil {
			return e
		}
		verification = &rm.Verification{Status: record.Status, Comparison: record.Comparison, VerifiedAt: time.Now().UTC(), DriftID: record.ID}
		if state == "succeeded" && record.Status != "compliant" {
			state = "verification_failed"
			code = "verification_mismatch"
		}
		if state == "succeeded" {
			_, _, e = s.remediationRevalidateAfterTx(ctx, tx, p, j.ActorID)
			if e != nil {
				state = "unknown"
				code = "state_changed_during_execution"
			}
		}
	} else if state == "succeeded" || state == "verification_failed" {
		state = "unknown"
		code = "verification_unavailable"
	}
	// A caller-supplied success cannot bypass expected/template/port checks.
	if state == "succeeded" {
		port, e := cd.AccessPortState(j.Target.TemplateID, evidence.Configuration, p.Action.Interface)
		if e != nil || !port.Safe || port.VLAN != p.Action.TargetVLAN || verification == nil || verification.Comparison.ExpectedDigest != p.Action.ExpectedDigest {
			state = "verification_failed"
			code = "verification_mismatch"
		}
	}
	code = safeRemediationError(code)
	if state == "verification_failed" && code == "" {
		code = "verification_mismatch"
	}
	raw, _ := json.Marshal(verification)
	if verification == nil {
		raw = nil
	}
	if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET state=$2,error_code=$3,finished_at=now(),claim_token=NULL,snapshot_id=$4 WHERE id=$1`, j.ID, state, code, afterID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE remediation_proposals SET state=$2,error_code=$3,after_snapshot_id=$4,verification=$5 WHERE id=$1`, p.ID, state, code, afterID, raw); err != nil {
		return err
	}
	if err = automationEvent(ctx, tx, j.ID, state); err != nil {
		return err
	}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.verification.finished", state, p.ID, access.Meta{}); err != nil {
		return err
	}
	if err = automationAudit(ctx, tx, j.ActorID, "remediation.job.finished", state, p.ID, access.Meta{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Finish validation intentionally ignores latest observed drift (our own
// read-back has just superseded it), and approval expiry while verification runs.
func (s *Store) remediationRevalidateAfterTx(ctx context.Context, tx pgx.Tx, p rm.Proposal, user string) (au.Target, cd.Baseline, error) {
	var target au.Target
	var baseline cd.Baseline
	a, _, err := s.remediationIdentityTx(ctx, tx, p.Action.AssetID, p.Action.Interface)
	if err != nil {
		return target, baseline, err
	}
	if a.AssetRevision != p.Action.AssetRevision || a.BindingID != p.Action.BindingID || a.BindingDigest != p.Action.BindingDigest || a.InterfaceID != p.Action.InterfaceID || a.TargetRevision != p.Action.TargetRevision || a.TargetDigest != p.Action.TargetDigest || a.PolicyDigest != p.Action.PolicyDigest || a.TemplateDigest != p.Action.TemplateDigest || s.remediationPolicy.Authorize(user, p.Action) != nil {
		return target, baseline, rm.ErrConflict
	}
	baseline, err = readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, a.AssetID))
	if err == nil && (baseline.Revision != p.Action.BaselineRevision || baseline.SnapshotID != p.Action.ExpectedSnapshotID || baseline.NormalizedDigest != p.Action.ExpectedDigest) {
		err = rm.ErrConflict
	}
	return target, baseline, err
}
func safeRemediationError(code string) string {
	switch code {
	case "", "approval_validation_failed", "preflight_unavailable", "preflight_changed", "execution_uncertain", "verification_unavailable", "verification_mismatch", "state_changed_during_execution", "worker_lost", "cancelled":
		return code
	default:
		return "execution_uncertain"
	}
}
