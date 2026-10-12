package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"golang.org/x/crypto/ssh"
	"strings"
	"sync"
	"testing"
	"time"
)

func automationFixture(t *testing.T) (*Store, context.Context, access.Principal, au.Target) {
	t.Helper()
	s := integrationStore(t)
	_, p := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), p)
	vault, err := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureAutomation(ctx, true, vault); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO assets(id,kind,canonical_name) VALUES('asset_ansible','network_device','Reviewed switch')`); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	in := au.TargetInput{ExpectedRevision: 0, AssetRevision: 1, Host: "192.0.2.20", Port: 22, TemplateID: "cisco_ios_read_config_v1", CredentialRef: "switch_readonly", SSHHostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), Enabled: true, Reason: "reviewed lab switch"}
	target, err := s.PutAutomationTarget(ctx, "asset_ansible", in)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AutomationWorker(ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{in.TemplateID}, Versions: map[string]string{"ansible-core": "test"}}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, p, target
}
func automationInput(target au.Target) au.Input {
	return au.Input{TargetRevision: target.Revision, TimeoutSeconds: 30, Reason: "read configuration for comparison"}
}
func TestAutomationDurableClaimSnapshotEncryptionAndAudit(t *testing.T) {
	s, ctx, p, target := automationFixture(t)
	in := automationInput(target)
	j, replay, err := s.CreateAutomation(ctx, target.AssetID, in, "config-read-one")
	if err != nil || replay || j.State != "queued" {
		t.Fatal(j, replay, err)
	}
	duplicate, replay, err := s.CreateAutomation(ctx, target.AssetID, in, "config-read-one")
	if err != nil || !replay || duplicate.ID != j.ID {
		t.Fatal("idempotency", err)
	}
	in.Reason = "different reason"
	if _, _, err = s.CreateAutomation(ctx, target.AssetID, in, "config-read-one"); !errors.Is(err, au.ErrConflict) {
		t.Fatal("key reuse", err)
	}
	if _, _, err = s.CreateAutomation(ctx, target.AssetID, automationInput(target), "second-active-job"); !errors.Is(err, au.ErrConflict) {
		t.Fatal("one active per asset", err)
	}
	var claims []au.Job
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			v, e := s.ClaimAutomation(ctx, "worker_one")
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				t.Error(e)
			}
			if v.ID != "" {
				claims = append(claims, v)
			}
		})
	}
	wg.Wait()
	if len(claims) != 1 || claims[0].ID != j.ID {
		t.Fatal("exclusive claim", claims)
	}
	claimed := claims[0]
	wrongJob := claimed
	wrongJob.AssetID = "other_asset"
	if err = s.FinishAutomation(ctx, wrongJob, "succeeded", au.Result{Configuration: "hostname wrong", Facts: json.RawMessage(`{}`)}); !errors.Is(err, au.ErrConflict) {
		t.Fatal("result can be attached to wrong Asset", err)
	}
	result := au.Result{Configuration: "hostname switch\nusername ops secret sensitive-password\ninterface Gi0/12\n switchport access vlan 20\n", Facts: json.RawMessage(`{"ansible_net_model":"C9300"}`)}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", result); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", result); !errors.Is(err, au.ErrConflict) {
		t.Fatal("duplicate completion", err)
	}
	snapshots, err := s.AutomationSnapshots(ctx, target.AssetID, 50, 0)
	if err != nil || len(snapshots) != 1 || snapshots[0].Digest != au.Digest(result.Configuration) {
		t.Fatal(snapshots, err)
	}
	var cipher []byte
	if err = s.pool.QueryRow(ctx, `SELECT ciphertext FROM automation_snapshots WHERE id=$1`, snapshots[0].ID).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("sensitive-password")) {
		t.Fatal("configuration leaked in database")
	}
	content, err := s.AutomationSnapshotContent(ctx, snapshots[0].ID)
	if err != nil || content.Configuration != result.Configuration {
		t.Fatal(content, err)
	}
	events, err := s.AutomationEvents(ctx, j.ID, 50, 0)
	if err != nil || len(events) != 3 {
		t.Fatal(events, err)
	}
	raw, _ := json.Marshal(events)
	if bytes.Contains(raw, []byte("sensitive")) {
		t.Fatal("secret in events")
	}
	p.User.Role = "viewer" // DB is authoritative, a forged local principal cannot grant rights.
	if _, err = s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, p.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AutomationSnapshotContent(access.WithPrincipal(ctx, p), snapshots[0].ID); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("raw snapshot ACL", err)
	}
	wrong, _ := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{32}, 32)))
	if err = s.ConfigureAutomation(ctx, false, wrong); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action IN ('automation.target.update','automation.job.create','automation.job.finish','automation.snapshot.read')`).Scan(&count); err != nil || count != 4 {
		t.Fatal("audit", count, err)
	}
}
func TestAutomationCancellationRevocationRetargetAndOrphans(t *testing.T) {
	for _, test := range []string{"queued_cancel", "running_cancel", "revoked_session", "retarget", "merged_asset", "expired_worker", "orphan", "deadline", "bad_result"} {
		t.Run(test, func(t *testing.T) {
			s, ctx, p, target := automationFixture(t)
			if test == "expired_worker" {
				_, err := s.pool.Exec(ctx, `UPDATE automation_workers SET last_seen=now()-interval '1 minute'`)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = s.CreateAutomation(ctx, target.AssetID, automationInput(target), "one"); !errors.Is(err, au.ErrUnavailable) {
					t.Fatal(err)
				}
				return
			}
			j, _, err := s.CreateAutomation(ctx, target.AssetID, automationInput(target), "one")
			if err != nil {
				t.Fatal(err)
			}
			if test == "queued_cancel" {
				v, err := s.CancelAutomation(ctx, j.ID)
				if err != nil || v.State != "cancelled" {
					t.Fatal(v, err)
				}
				v, err = s.ClaimAutomation(ctx, "worker_one")
				if err != nil || v.ID != "" {
					t.Fatal("cancelled dispatched", err)
				}
				return
			}
			if test == "revoked_session" {
				if _, err = s.pool.Exec(ctx, `DELETE FROM access_sessions WHERE token_hash=$1`, p.SessionHash); err != nil {
					t.Fatal(err)
				}
			}
			if test == "retarget" {
				in := target.TargetInput
				in.ExpectedRevision = target.Revision
				in.Host = "192.0.2.21"
				if _, err = s.PutAutomationTarget(ctx, target.AssetID, in); err != nil {
					t.Fatal(err)
				}
			}
			if test == "merged_asset" {
				if _, err = s.pool.Exec(ctx, `UPDATE assets SET lifecycle_status='merged',revision=revision+1 WHERE id=$1`, target.AssetID); err != nil {
					t.Fatal(err)
				}
			}
			claimed, err := s.ClaimAutomation(ctx, "worker_one")
			if err != nil {
				t.Fatal(err)
			}
			if test == "revoked_session" || test == "retarget" || test == "merged_asset" {
				if claimed.ID != "" {
					t.Fatal("stale authorization/target dispatched")
				}
				v, err := s.AutomationJob(ctx, j.ID)
				if err != nil || v.State != "failed" || v.ErrorCode != "authorization_or_target_changed" {
					t.Fatal(v, err)
				}
				return
			}
			result := au.Result{Configuration: "hostname test\n", Facts: json.RawMessage(`{}`)}
			want := "succeeded"
			if test == "running_cancel" {
				if _, err = s.CancelAutomation(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
				want = "cancelled"
			}
			if test == "orphan" {
				if _, err = s.pool.Exec(ctx, `UPDATE automation_jobs SET deadline=now()-interval '1 minute' WHERE id=$1`, j.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = s.ClaimAutomation(ctx, "worker_one"); err != nil {
					t.Fatal(err)
				}
				v, err := s.AutomationJob(ctx, j.ID)
				if err != nil || v.State != "failed" || v.ErrorCode != "worker_lost" {
					t.Fatal(v, err)
				}
				if err = s.FinishAutomation(ctx, claimed, "succeeded", result); !errors.Is(err, au.ErrConflict) {
					t.Fatal("stale result accepted", err)
				}
				return
			}
			if test == "deadline" {
				if _, err = s.pool.Exec(ctx, `UPDATE automation_jobs SET deadline=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
					t.Fatal(err)
				}
				want = "timed_out"
			}
			if test == "bad_result" {
				result.Configuration = ""
				want = "failed"
			}
			if err = s.FinishAutomation(ctx, claimed, "succeeded", result); err != nil {
				t.Fatal(err)
			}
			v, err := s.AutomationJob(ctx, j.ID)
			if err != nil || v.State != want || v.SnapshotID != "" {
				t.Fatal(v, err)
			}
		})
	}
}
func TestAutomationRunningRevocationDiscardsResult(t *testing.T) {
	s, ctx, p, target := automationFixture(t)
	j, _, err := s.CreateAutomation(ctx, target.AssetID, automationInput(target), "one")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAutomation(ctx, "worker_one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE access_users SET disabled=true WHERE id=$1`, p.User.ID); err != nil {
		t.Fatal(err)
	}
	valid, err := s.AutomationRunning(ctx, claimed)
	if err != nil || valid {
		t.Fatal("live revocation", err)
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", au.Result{Configuration: "secret configuration", Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	final, err := s.AutomationJob(ctx, j.ID)
	if err != nil || final.State != "failed" || final.SnapshotID != "" {
		t.Fatal(final, err)
	}
	var snapshots int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM automation_snapshots`).Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatal(snapshots, err)
	}
}
func TestAutomationWorkerSchemaCheckDoesNotDisconnectAgent(t *testing.T) {
	s, ctx, _, _ := automationFixture(t)
	if err := s.Ingest(ctx, registration("automation-reference", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAutomationSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var connected bool
	if err := s.pool.QueryRow(ctx, `SELECT connected FROM agents WHERE agent_id='automation-reference'`).Scan(&connected); err != nil || !connected {
		t.Fatal("worker changed Agent connection", err)
	}
}

func TestAutomationOperatorOwnershipAndAdministratorOnlyContent(t *testing.T) {
	s, ctx, admin, target := automationFixture(t)
	hash, err := access.HashPassword(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AccessCreateUser(ctx, admin, "read_operator", "operator", hash, false, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	svc := access.New(s, access.Config{Mode: "required", SessionTTL: time.Hour})
	operator, err := svc.Login(ctx, "read_operator", fixturePassword, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	operatorCtx := access.WithPrincipal(ctx, operator)
	if _, err = s.PutAutomationTarget(operatorCtx, target.AssetID, target.TargetInput); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("operator changed trusted binding", err)
	}
	adminJob, _, err := s.CreateAutomation(ctx, target.AssetID, automationInput(target), "admin-job")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelAutomation(operatorCtx, adminJob.ID); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("operator cancelled someone else's job", err)
	}
	if _, err = s.CancelAutomation(ctx, adminJob.ID); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.CreateAutomation(operatorCtx, target.AssetID, automationInput(target), "operator-job")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAutomation(ctx, "worker_one")
	if err != nil || claimed.ID != job.ID {
		t.Fatal(err)
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", au.Result{Configuration: "hostname ops\n", Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	finished, err := s.AutomationJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AutomationSnapshotContent(operatorCtx, finished.SnapshotID); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("operator read secret configuration", err)
	}
	var resource string
	if err = s.pool.QueryRow(ctx, `SELECT resource FROM access_audit WHERE action='automation.job.create' AND actor_id=$1 ORDER BY id DESC LIMIT 1`, operator.User.ID).Scan(&resource); err != nil || resource != job.ID {
		t.Fatal("audit does not identify launched job", resource, err)
	}
}
