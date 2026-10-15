package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

type aiWorkerFixture struct{ calls atomic.Int32 }

func (p *aiWorkerFixture) Analyze(_ context.Context, preview analysisai.Preview) (analysisai.Result, error) {
	p.calls.Add(1)
	return analysisai.Result{Summary: "Fixture result, not a live model.", Observations: []analysisai.Statement{{Text: "Observed fixture warning.", Refs: []string{preview.Payload.Facts[0].Ref}}}, Hypotheses: []analysisai.Hypothesis{}, Recommendations: []analysisai.Recommendation{}, Limitations: []string{"Local fixture provider."}}, nil
}
func TestAIWorkerConsumesReviewedJobOnce(t *testing.T) {
	s, cfg, incident, preview := aiFixture(t)
	ctx := access.WithPrincipal(t.Context(), aiActorContext(t, s))
	job, _, err := s.CreateJob(ctx, incident, analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash, Confirmed: true}, "worker", cfg)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	provider := &aiWorkerFixture{}
	go func() {
		defer close(done)
		analysisai.Run(workerCtx, s, provider, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(6 * time.Second)
	succeeded := false
	for time.Now().Before(deadline) {
		saved, e := s.Job(ctx, job.ID)
		if e == nil && saved.State == "succeeded" {
			succeeded = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AI worker did not stop")
	}
	if !succeeded || provider.calls.Load() != 1 {
		t.Fatal("AI worker did not persist exactly one reviewed analysis")
	}
}

func aiFixture(t *testing.T) (*Store, analysisai.Config, string, analysisai.Preview) {
	t.Helper()
	s, ctx, now := healthFixture(t)
	for i := 0; i < 3; i++ {
		if err := s.Ingest(ctx, healthEnvelope(fmt.Sprint("ai", i), i, now.Add(time.Duration(i-2)*3*time.Second), 85, "linux")); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{RuleID: "cpu.sustained"})
	if err != nil || len(items) != 1 {
		t.Fatal("AI fixture incident missing")
	}
	cfg := analysisai.Config{Enabled: true, Model: "fixture-model", APIKey: "not-real", Timeout: time.Second}
	preview, err := s.CreatePreview(ctx, items[0].ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Retrieve the active fixture actor without constructing a second bootstrap.
	return s, cfg, items[0].ID, preview
}
func aiActorContext(t *testing.T, s *Store) access.Principal {
	t.Helper()
	var tokenHash []byte
	var user string
	if err := s.pool.QueryRow(t.Context(), `SELECT user_id,token_hash FROM access_sessions LIMIT 1`).Scan(&user, &tokenHash); err != nil {
		t.Fatal(err)
	}
	p, err := s.AccessSession(t.Context(), tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestAIPreviewConfirmedJobIdempotencyAndValidatedResult(t *testing.T) {
	s, cfg, incident, preview := aiFixture(t)
	ctx := access.WithPrincipal(t.Context(), aiActorContext(t, s))
	body := analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash, Confirmed: true}
	job, replay, err := s.CreateJob(ctx, incident, body, "one-analysis", cfg)
	if err != nil || replay {
		t.Fatal(err)
	}
	same, hit, err := s.CreateJob(ctx, incident, body, "one-analysis", cfg)
	if err != nil || !hit || same.ID != job.ID {
		t.Fatal("job replay duplicated")
	}
	claimed, p, err := s.ClaimJob(ctx, cfg)
	if err != nil || claimed.ID != job.ID || p.Hash != preview.Hash {
		t.Fatal("wrong claimed preview")
	}
	result := &analysisai.Result{Summary: "Сигнал подтверждён измерением.", Observations: []analysisai.Statement{{Text: "Нагрузка повышена.", Refs: []string{p.Payload.Facts[0].Ref}}}, Hypotheses: []analysisai.Hypothesis{}, Recommendations: []analysisai.Recommendation{}, Limitations: []string{"Причина неизвестна."}}
	if err = s.FinishJob(ctx, job.ID, result, "", false); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Job(ctx, job.ID)
	if err != nil || saved.State != "succeeded" || saved.Result == nil {
		t.Fatal("AI result not saved")
	}
	if _, _, err = s.CreateJob(ctx, incident, body, "different-key", cfg); !errors.Is(err, analysisai.ErrConflict) {
		t.Fatal("preview sent twice")
	}
}
func TestAIExpiryRevocationAndRestartDoNotSendAgain(t *testing.T) {
	s, cfg, incident, preview := aiFixture(t)
	actor := aiActorContext(t, s)
	ctx := access.WithPrincipal(t.Context(), actor)
	body := analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash, Confirmed: true}
	job, _, err := s.CreateJob(ctx, incident, body, "restart", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimJob(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverJobs(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Job(ctx, job.ID)
	if saved.State != "uncertain" {
		t.Fatal("potentially sent analysis replayed after restart")
	}
	if _, _, err = s.ClaimJob(ctx, cfg); !errors.Is(err, analysisai.ErrNotFound) {
		t.Fatal("uncertain analysis resent")
	}
	second, err := s.CreatePreview(ctx, incident, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE ai_previews SET expires_at=now()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateJob(ctx, incident, analysisai.Create{PreviewID: second.ID, RequestHash: second.Hash, Confirmed: true}, "expired", cfg); !errors.Is(err, analysisai.ErrConflict) {
		t.Fatal("expired preview sent")
	}
	third, err := s.CreatePreview(ctx, incident, cfg)
	if err != nil {
		t.Fatal(err)
	}
	queued, _, err := s.CreateJob(ctx, incident, analysisai.Create{PreviewID: third.ID, RequestHash: third.Hash, Confirmed: true}, "logout", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AccessLogout(ctx, actor, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimJob(ctx, cfg); !errors.Is(err, analysisai.ErrNotFound) {
		t.Fatal("revoked actor sent analysis")
	}
	saved, _ = s.Job(ctx, queued.ID)
	if saved.State != "failed" {
		t.Fatal("revoked actor job not failed")
	}
}
func TestAIConfirmationDisabledHashAndAuditFailure(t *testing.T) {
	s, cfg, incident, preview := aiFixture(t)
	ctx := access.WithPrincipal(t.Context(), aiActorContext(t, s))
	body := analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash, Confirmed: true}
	disabled := cfg
	disabled.Enabled = false
	if _, _, err := s.CreateJob(ctx, incident, body, "disabled", disabled); !errors.Is(err, analysisai.ErrDisabled) {
		t.Fatal("disabled AI sent")
	}
	body.Confirmed = false
	if _, _, err := s.CreateJob(ctx, incident, body, "unconfirmed", cfg); !errors.Is(err, analysisai.ErrInvalid) {
		t.Fatal("unconfirmed AI sent")
	}
	body.Confirmed = true
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit ADD CONSTRAINT reject_ai CHECK(action<>'ai_analysis_confirmed')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateJob(ctx, incident, body, "audit-fail", cfg); err == nil {
		t.Fatal("audit SQL failure absent")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ai_analyses`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial AI job persisted")
	}
}
