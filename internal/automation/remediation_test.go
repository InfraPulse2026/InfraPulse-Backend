package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restoreFixture() *RestoreAccessVLAN {
	return &RestoreAccessVLAN{TemplateID: RestoreTemplateID, TemplateDigest: RestoreTemplateDigest(), Interface: "GigabitEthernet0/12", CurrentVLAN: 30, TargetVLAN: 20}
}

func TestRestoreTemplateIsSeparateAndExactlyPinned(t *testing.T) {
	if RestoreTemplate().ReadOnly || RestoreTemplate().Platform != "cisco.ios.ios" || len(RestoreTemplateDigest()) != 64 {
		t.Fatal("write template identity is incomplete")
	}
	if _, ok := Lookup(RestoreTemplateID); ok {
		t.Fatal("write workflow is selectable by ordinary read-only jobs")
	}
	if RestoreTemplateDigest() != Digest(string(runnerScript)) {
		t.Fatal("approved digest does not pin executable artifact")
	}
	if restoreFixture().Validate() != nil {
		t.Fatal("valid typed restoration rejected")
	}
	for name, mutate := range map[string]func(*RestoreAccessVLAN){
		"unknown_template":    func(v *RestoreAccessVLAN) { v.TemplateID = "custom_playbook" },
		"changed_template":    func(v *RestoreAccessVLAN) { v.TemplateDigest = strings.Repeat("0", 64) },
		"interface_injection": func(v *RestoreAccessVLAN) { v.Interface += "; reboot" },
		"interface_range":     func(v *RestoreAccessVLAN) { v.Interface = "range GigabitEthernet0/12-14" },
		"alias":               func(v *RestoreAccessVLAN) { v.Interface = "Gi0/12" },
		"management":          func(v *RestoreAccessVLAN) { v.Interface = "Management0" },
		"lag":                 func(v *RestoreAccessVLAN) { v.Interface = "Port-channel1" },
		"virtual":             func(v *RestoreAccessVLAN) { v.Interface = "Vlan20" },
		"subinterface":        func(v *RestoreAccessVLAN) { v.Interface = "GigabitEthernet0/12.20" },
		"no_current_vlan":     func(v *RestoreAccessVLAN) { v.CurrentVLAN = 0 },
		"no_target_vlan":      func(v *RestoreAccessVLAN) { v.TargetVLAN = 0 },
		"invalid_vlan":        func(v *RestoreAccessVLAN) { v.TargetVLAN = 4095 },
		"already_compliant":   func(v *RestoreAccessVLAN) { v.TargetVLAN = v.CurrentVLAN },
	} {
		t.Run(name, func(t *testing.T) {
			v := restoreFixture()
			mutate(v)
			if v.Validate() == nil {
				t.Fatal("unsafe typed restoration accepted")
			}
		})
	}
}

func TestRestoreExecutionParametersNeverAppearInJobAPI(t *testing.T) {
	j := Job{ProposalID: "proposal_one", ActionDigest: strings.Repeat("a", 64), Restore: restoreFixture()}
	raw, err := json.Marshal(j)
	if err != nil || !bytes.Contains(raw, []byte("proposal_one")) || bytes.Contains(raw, []byte("current_vlan")) || bytes.Contains(raw, []byte("GigabitEthernet")) || bytes.Contains(raw, []byte("template_digest")) {
		t.Fatal("execution parameters leaked in ordinary job API", err)
	}
}

func TestInvalidRestorationAndNonCiscoTargetNeverLoadCredentials(t *testing.T) {
	p := Process{}
	j := Job{TemplateID: RestoreTemplateID, Target: Target{TargetInput: targetFixture(t)}, Restore: restoreFixture()}
	j.Restore.Interface = "GigabitEthernet0/12\nreload"
	if result := p.Execute(t.Context(), j); result.ErrorCode != "invalid_result" {
		t.Fatal("invalid restoration reached destination/credentials", result)
	}
	j.Restore = restoreFixture()
	j.Target.TemplateID = "arista_eos_read_config_v1"
	if result := p.Execute(t.Context(), j); result.ErrorCode != "invalid_result" {
		t.Fatal("unsupported vendor reached destination/credentials", result)
	}
}

type remediationWorkerRepo struct {
	workerRepo
	calls  atomic.Int32
	state  string
	result Result
	block  bool
}

func (r *remediationWorkerRepo) ExecuteRemediation(ctx context.Context, _ Executor, _ Job) (string, Result) {
	r.calls.Add(1)
	if r.block {
		<-ctx.Done()
		return "unknown", Result{ErrorCode: "execution_uncertain"}
	}
	return r.state, r.result
}

func TestWorkerRemediationRequiresLiveAuthorizationAndRepository(t *testing.T) {
	j := Job{ProposalID: "proposal_one", Restore: restoreFixture(), TimeoutSeconds: 30}
	exec := &blockingExecutor{}
	repo := &remediationWorkerRepo{state: "no_action"}
	state, _ := executeJob(t.Context(), repo, exec, j)
	if state != "stale" || repo.calls.Load() != 0 || exec.calls.Load() != 0 {
		t.Fatal("unauthorized remediation reached execution")
	}
	repo.valid.Store(true)
	base := &workerRepo{}
	base.valid.Store(true)
	state, _ = executeJob(t.Context(), base, exec, j)
	if state != "stale" || exec.calls.Load() != 0 {
		t.Fatal("unsupported remediation repository fell back to read execution")
	}
	j.Restore = nil
	state, _ = executeJob(t.Context(), repo, exec, j)
	if state != "stale" || repo.calls.Load() != 0 {
		t.Fatal("missing immutable execution parameters accepted")
	}
}

func TestWorkerPreservesVerifiedRemediationOutcomes(t *testing.T) {
	for _, wanted := range []string{"succeeded", "stale", "no_action", "verification_failed", "unknown", "failed", "expired"} {
		t.Run(wanted, func(t *testing.T) {
			repo := &remediationWorkerRepo{state: wanted, result: Result{Configuration: "hostname test\n", Facts: json.RawMessage(`{}`)}}
			repo.valid.Store(true)
			exec := &blockingExecutor{}
			state, _ := executeJob(t.Context(), repo, exec, Job{ProposalID: "proposal_one", Restore: restoreFixture(), TimeoutSeconds: 30})
			if state != wanted || repo.calls.Load() != 1 || exec.calls.Load() != 0 {
				t.Fatal("repository outcome lost or unguarded direct execution", state)
			}
		})
	}
	repo := &remediationWorkerRepo{state: "succeeded", result: Result{Facts: json.RawMessage(`{}`)}}
	repo.valid.Store(true)
	state, _ := executeJob(t.Context(), repo, &blockingExecutor{}, Job{ProposalID: "proposal_one", Restore: restoreFixture(), TimeoutSeconds: 30})
	if state != "unknown" {
		t.Fatal("write acknowledgement without read-back counted as success")
	}
}

func TestWorkerInterruptedRemediationIsUnknownWithoutReplay(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		repo := &remediationWorkerRepo{block: true}
		repo.valid.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		j := Job{ProposalID: "proposal_one", Restore: restoreFixture(), TimeoutSeconds: 30}
		if timeout {
			j.TimeoutSeconds = 0
		} else {
			time.AfterFunc(20*time.Millisecond, cancel)
		}
		state, result := executeJob(ctx, repo, &blockingExecutor{}, j)
		cancel()
		if state != "unknown" || result.ErrorCode != "execution_uncertain" || repo.calls.Load() != 1 {
			t.Fatal("interrupted remediation has misleading outcome or replay", state, result)
		}
	}
}
