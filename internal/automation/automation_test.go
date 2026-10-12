package automation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func targetFixture(t *testing.T) TargetInput {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return TargetInput{AssetRevision: 1, Host: "192.0.2.22", Port: 22, TemplateID: Templates()[0].ID, CredentialRef: "read_only", SSHHostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), Reason: "reviewed device", Enabled: true}
}
func TestClosedTargetValidation(t *testing.T) {
	good := targetFixture(t)
	if good.Validate() != nil {
		t.Fatal("valid target rejected")
	}
	for _, mutate := range []func(*TargetInput){func(v *TargetInput) { v.Host = "switch.example.test" }, func(v *TargetInput) { v.Host = "192.0.2.22; reboot" }, func(v *TargetInput) { v.Host = "0.0.0.0" }, func(v *TargetInput) { v.Port = 65536 }, func(v *TargetInput) { v.TemplateID = "arbitrary_playbook" }, func(v *TargetInput) { v.CredentialRef = "../../secret" }, func(v *TargetInput) { v.AssetRevision = 0 }, func(v *TargetInput) { v.SSHHostKey += "\n" + good.SSHHostKey }} {
		in := good
		mutate(&in)
		if in.Validate() == nil {
			t.Fatal("unsafe target accepted", in)
		}
	}
	for _, template := range Templates() {
		if !template.ReadOnly {
			t.Fatal("write template included")
		}
	}
}
func TestCredentialsAreClosedAndPathBounded(t *testing.T) {
	dir := t.TempDir()
	p := Process{CredentialsDir: dir}
	if err := os.WriteFile(filepath.Join(dir, "read_only.json"), []byte(`{"username":"observer","password":"test-only"}`), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := p.credentials("read_only")
	if err != nil || value.Username != "observer" {
		t.Fatal(err)
	}
	for _, ref := range []string{"../read_only", "read_only.json", "/read_only", ""} {
		if _, err = p.credentials(ref); err == nil {
			t.Fatal("unsafe credential path accepted", ref)
		}
	}
	for _, body := range []string{`{"username":"observer","password":"x","ansible_command":"reboot"}`, `{"username":"observer","password":"x"} {}`, `{"username":"observer","password":""}`} {
		if err = os.WriteFile(filepath.Join(dir, "bad.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = p.credentials("bad"); err == nil {
			t.Fatal("unsafe credential contents")
		}
	}
}
func TestDestinationPolicyPrecedesSecretsAndExecution(t *testing.T) {
	p := Process{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	v := targetFixture(t)
	v.Host = "198.51.100.1"
	result := p.Execute(t.Context(), Job{Target: Target{TargetInput: v}})
	if result.ErrorCode != "destination_denied" {
		t.Fatal(result)
	}
}
func TestOutputAndSnapshotBounds(t *testing.T) {
	w := boundedOutput{max: 4}
	_, _ = w.Write([]byte("1234"))
	_, _ = w.Write(bytes.Repeat([]byte("secret"), 100))
	if !w.exceeded || w.data.String() != "1234" {
		t.Fatal("output not bounded")
	}
	j := Job{Target: Target{TargetInput: targetFixture(t)}, ClaimToken: "private-worker-token"}
	raw, _ := json.Marshal(j)
	if bytes.Contains(raw, []byte("private-worker-token")) || bytes.Contains(raw, []byte("credential_ref")) {
		t.Fatal("private execution data in REST")
	}
	for _, v := range []Result{{Configuration: "", Facts: json.RawMessage(`{}`)}, {Configuration: "x", Facts: json.RawMessage(`invalid`)}, {Configuration: strings.Repeat("x", MaxConfiguration+1), Facts: json.RawMessage(`{}`)}} {
		if v.Validate() == nil {
			t.Fatal("bad result accepted")
		}
	}
}

type workerRepo struct{ valid atomic.Bool }

func (r *workerRepo) AutomationWorker(context.Context, WorkerStatus) error { return nil }
func (r *workerRepo) ClaimAutomation(context.Context, string) (Job, error) { return Job{}, nil }
func (r *workerRepo) AutomationRunning(context.Context, Job) (bool, error) {
	return r.valid.Load(), nil
}
func (r *workerRepo) FinishAutomation(context.Context, Job, string, Result) error { return nil }

type blockingExecutor struct{ calls atomic.Int32 }

func (e *blockingExecutor) Probe(context.Context) WorkerStatus { return WorkerStatus{Available: true} }
func (e *blockingExecutor) Execute(ctx context.Context, _ Job) Result {
	e.calls.Add(1)
	<-ctx.Done()
	return Result{ErrorCode: "runner_failed"}
}
func TestWorkerCancellationAndPreDispatchAuthorization(t *testing.T) {
	repo := &workerRepo{}
	exec := &blockingExecutor{}
	state, _ := executeJob(t.Context(), repo, exec, Job{TimeoutSeconds: 30})
	if state != "failed" || exec.calls.Load() != 0 {
		t.Fatal("dispatched without live authorization")
	}
	repo.valid.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	state, _ = executeJob(ctx, repo, exec, Job{TimeoutSeconds: 30})
	if state != "cancelled" || exec.calls.Load() != 1 {
		t.Fatal(state)
	}
	state, _ = executeJob(t.Context(), repo, exec, Job{TimeoutSeconds: 0})
	if state != "timed_out" {
		t.Fatal(state)
	}
}
