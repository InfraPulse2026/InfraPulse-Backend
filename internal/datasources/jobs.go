package datasources

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"log/slog"
	"sort"
	"time"
)

var ErrBusy = errors.New("source operation already active")
var ErrDisabled = errors.New("source disabled")
var ErrLease = errors.New("source job lease no longer valid")

type Job struct {
	ID             string     `json:"id"`
	SourceID       string     `json:"source_id"`
	Operation      string     `json:"operation"`
	Trigger        string     `json:"trigger"`
	State          string     `json:"state"`
	SourceRevision int64      `json:"source_revision"`
	Attempts       int        `json:"attempts"`
	Code           string     `json:"code"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	ActorID        *string    `json:"actor_id"`
	LeaseToken     string     `json:"-"`
}

type Event struct {
	ID      int64     `json:"id"`
	JobID   string    `json:"job_id"`
	Attempt int       `json:"attempt"`
	State   string    `json:"state"`
	Code    string    `json:"code"`
	At      time.Time `json:"at"`
}

// Request credentials and cursor are private adapter inputs, never REST results.
type Request struct {
	Job            Job
	Source         Source
	Credential     []byte                    `json:"-"`
	Cursor         json.RawMessage           `json:"-"`
	PendingSignals []externalmonitor.Pending `json:"-"`
}
type Result struct {
	Cursor       json.RawMessage        `json:"-"`
	Observations []assets.Observation   `json:"-"`
	Monitoring   *externalmonitor.Batch `json:"-"`
	Prometheus   *prommodel.Batch       `json:"-"`
}
type Handler func(context.Context, Request) (Result, error)
type Operation struct {
	Kind, Name string
	ReadOnly   bool
	Run        Handler
}

// Registry is immutable after construction. Only read-only test/sync operations
// may enter this retryable queue; remediation stays in the command pipeline.
type Registry struct{ ops map[string]Operation }

func NewRegistry(ops ...Operation) (*Registry, error) {
	r := &Registry{ops: map[string]Operation{}}
	for _, op := range ops {
		if (op.Kind != "COLLECTOR" && op.Kind != "ZABBIX" && op.Kind != "PROMETHEUS" && op.Kind != "ANSIBLE") || (op.Name != "test" && op.Name != "sync") || !op.ReadOnly || op.Run == nil {
			return nil, ErrInvalid
		}
		key := op.Kind + ":" + op.Name
		if _, ok := r.ops[key]; ok {
			return nil, ErrConflict
		}
		r.ops[key] = op
	}
	return r, nil
}
func (r *Registry) Lookup(kind, name string) (Operation, bool) {
	if r == nil {
		return Operation{}, false
	}
	op, ok := r.ops[kind+":"+name]
	return op, ok
}

func (r *Registry) With(ops ...Operation) (*Registry, error) {
	all := []Operation{}
	if r != nil {
		for _, op := range r.ops {
			all = append(all, op)
		}
	}
	return NewRegistry(append(all, ops...)...)
}
func (r *Registry) SyncKinds() []string {
	items := []string{}
	for _, kind := range []string{"COLLECTOR", "ZABBIX", "PROMETHEUS", "ANSIBLE"} {
		if _, ok := r.Lookup(kind, "sync"); ok {
			items = append(items, kind)
		}
	}
	return items
}
func (r *Registry) Capabilities(kind string) []string {
	items := Capabilities(kind)
	for _, name := range []string{"test", "sync"} {
		if _, ok := r.Lookup(kind, name); ok {
			items = append(items, name)
		}
	}
	sort.Strings(items)
	return items
}

// Failure carries only a fixed public code. Never persist or log adapter error
// strings: HTTP responses and URL errors can contain credentials.
type Failure struct {
	Code      string
	Retryable bool
}

func (e Failure) Error() string { return e.Code }
func Outcome(err error) (string, bool) {
	if err == nil {
		return "ok", false
	}
	if errors.Is(err, ErrNetworkPolicy) {
		return "network_policy_rejected", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true
	}
	if errors.Is(err, context.Canceled) {
		return "interrupted", true
	}
	var f Failure
	if errors.As(err, &f) {
		switch f.Code {
		case "connection_failed", "timeout", "rate_limited", "interrupted":
			return f.Code, f.Retryable
		case "invalid_credentials", "tls_failed", "collector_disconnected", "invalid_response", "network_policy_rejected", "redirect_blocked", "response_too_large", "unsupported_version", "api_rejected", "configuration_required", "selected_hosts_unavailable", "partial_response":
			return f.Code, false
		}
	}
	return "adapter_error", false
}

type Queue interface {
	ScheduleDataSources(context.Context) error
	ClaimDataSourceJob(context.Context) (*Request, error)
	FinishDataSourceJob(context.Context, Request, Result, error) error
}

func invoke(ctx context.Context, op Operation, request Request) (result Result, err error) {
	defer func() {
		if recover() != nil {
			err = Failure{Code: "adapter_error"}
		}
	}()
	return op.Run(ctx, request)
}

// Run uses two bounded workers. Database leases exclude overlapping source
// operations across workers; adapter HTTP clients must obey the supplied context.
func Run(ctx context.Context, q Queue, registry *Registry, log *slog.Logger) {
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				step, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := q.ScheduleDataSources(step)
				var request *Request
				if err == nil {
					request, err = q.ClaimDataSourceJob(step)
				}
				cancel()
				if err != nil {
					log.Error("data source queue unavailable")
					continue
				}
				if request == nil {
					continue
				}
				op, ok := registry.Lookup(request.Source.Type, request.Job.Operation)
				var result Result
				var runErr error
				if !ok {
					runErr = Failure{Code: "adapter_error"}
				} else {
					call, stop := context.WithTimeout(ctx, time.Duration(request.Source.Configuration.TimeoutSeconds)*time.Second)
					result, runErr = invoke(call, op, *request)
					if call.Err() != nil {
						runErr = call.Err()
					}
					stop()
				}
				clear(request.Credential)
				request.Credential = nil
				finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
				if err = q.FinishDataSourceJob(finish, *request, result, runErr); err != nil && !errors.Is(err, ErrLease) {
					log.Error("data source completion unavailable")
				}
				stop()
			}
		}()
	}
	<-done
	<-done
}
