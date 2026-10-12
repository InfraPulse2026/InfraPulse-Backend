package datasources

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestQueueRegistryRejectsSideEffectsAndDuplicateOperations(t *testing.T) {
	op := Operation{Kind: "ANSIBLE", Name: "sync", ReadOnly: false, Run: func(context.Context, Request) (Result, error) { return Result{}, nil }}
	if _, err := NewRegistry(op); err != ErrInvalid {
		t.Fatal("unsafe action entered retry queue", err)
	}
	op.ReadOnly = true
	r, err := NewRegistry(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("ANSIBLE", "test"); ok {
		t.Fatal("unsupported operation invented")
	}
	if len(r.SyncKinds()) != 1 || len(r.Capabilities("COLLECTOR")) != 1 {
		t.Fatal("capabilities confused with source type")
	}
	if _, err = NewRegistry(op, op); err != ErrConflict {
		t.Fatal("duplicate operation accepted", err)
	}
}
func TestQueueOutcomeDoesNotExposeSecrets(t *testing.T) {
	code, retry := Outcome(errors.New("https://user:private-token@server: response password=private-token"))
	if code != "adapter_error" || retry {
		t.Fatal("raw error exposed or retry invented", code, retry)
	}
	code, retry = Outcome(Failure{Code: "invalid_credentials", Retryable: true})
	if code != "invalid_credentials" || retry {
		t.Fatal("credentials error retried")
	}
	code, retry = Outcome(context.DeadlineExceeded)
	if code != "timeout" || !retry {
		t.Fatal("timeout policy")
	}
	request := Request{Credential: []byte("private-token"), Cursor: json.RawMessage(`{"internal":"private-token"}`), Job: Job{LeaseToken: "private-lease"}}
	raw, err := json.Marshal(request)
	if err != nil || strings.Contains(string(raw), "private-") {
		t.Fatal("worker inputs leaked", string(raw), err)
	}
}

func TestAdapterPanicIsContained(t *testing.T) {
	op := Operation{Run: func(context.Context, Request) (Result, error) { panic("private-token") }}
	_, err := invoke(t.Context(), op, Request{})
	code, retry := Outcome(err)
	if code != "adapter_error" || retry {
		t.Fatal("panic not contained", code)
	}
}
