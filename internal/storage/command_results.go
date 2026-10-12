package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

func projectCommandResult(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	r := e.GetCommandResult()
	if err := commands.ValidateResult(r); err != nil {
		return err
	}
	business, err := protojson.Marshal(r)
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(business)
	public := commands.ResultFromEnvelope(e)
	public.ErrorMessage = logbook.SanitizeText(public.ErrorMessage)
	public.ErrorCode = logbook.SanitizeText(public.ErrorCode)
	if public.Verification != nil {
		public.Verification.Evidence = logbook.SanitizeText(public.Verification.Evidence)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		return err
	}
	c, err := readCommand(tx.QueryRow(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE id=$1 FOR UPDATE`, r.CommandId))
	disposition := "unmatched_command"
	var link *string
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	apply := false
	state := commands.ResultState(r)
	reason := "agent_reported_" + state
	if err == nil {
		if c.AgentID != e.AgentId {
			disposition = "agent_mismatch"
		} else {
			link = &c.ID
			switch {
			case c.DispatchedAt == nil || c.State == "queued" || c.State == "expired":
				disposition = "not_dispatched"
			case state == "unspecified":
				disposition = "unknown_status"
			case !commands.Active(c.State):
				disposition = "terminal_already_recorded"
			case c.State == "uncertain" && (state == "accepted" || state == "running"):
				disposition = "late_progress_ignored"
			case c.State == "running" && state == "accepted":
				disposition = "progress_regression_ignored"
			default:
				apply = true
				disposition = "applied"
			}
			if apply && state == "success" {
				v := r.Verification
				desired := c.Operation == "enable"
				if c.Operation == "rollback" && v != nil {
					desired = v.ExpectedAdminUp
				} // The actual snapshot baseline is Agent-owned, not transmitted in advance.
				if v == nil || !v.Verified || v.ObservedAt == nil || r.StartedAt == nil || r.FinishedAt == nil || r.FinishedAt.AsTime().Before(r.StartedAt.AsTime()) || v.ObservedAdminUp == nil || *v.ObservedAdminUp != desired || v.ExpectedAdminUp != desired || v.InterfaceId != c.InterfaceID {
					state, reason = "uncertain", "success_not_verified"
					public.Uncertain = true
					encoded, _ = json.Marshal(public)
				}
			}
			if apply && (state == "accepted" || state == "running") && c.State == state {
				apply = false
				disposition = "duplicate_progress"
			}
			if !apply && disposition == "terminal_already_recorded" {
				var old []byte
				if err = tx.QueryRow(ctx, `SELECT result_fingerprint FROM controller_commands WHERE id=$1`, c.ID).Scan(&old); err != nil {
					return err
				}
				if string(old) == string(fingerprint[:]) {
					disposition = "duplicate_result"
				} else {
					disposition = "conflicting_terminal_ignored"
				}
			}
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO command_results(agent_id,message_id,reported_command_id,command_id,disposition,payload) VALUES($1,$2,$3,$4,$5,$6)`, e.AgentId, e.MessageId, r.CommandId, link, disposition, encoded); err != nil {
		return err
	}
	if apply {
		if _, err = tx.Exec(ctx, `UPDATE controller_commands SET state=$2,reason=$3,result=$4,result_fingerprint=$5,updated_at=now() WHERE id=$1`, c.ID, state, reason, encoded, fingerprint[:]); err != nil {
			return err
		}
		if err = commandEvent(ctx, tx, c.ID, state, reason); err != nil {
			return err
		}
		if err = updateSnapshotResult(ctx, tx, c, public, state); err != nil {
			return err
		}
	}
	return nil
}
