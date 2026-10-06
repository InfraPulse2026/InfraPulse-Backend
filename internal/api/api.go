package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

type Repository interface {
	Ping(context.Context) error
	Agents(context.Context) ([]storage.Agent, error)
	Devices(context.Context, string, string, int, int) ([]storage.Device, error)
	Device(context.Context, string, string) (storage.Device, error)
	Metrics(context.Context, string, string, int) ([]storage.Telemetry, error)
	UpdateMonitoring(context.Context, string, string, monitoring.Policy) error
}
type API struct {
	repo Repository
	log  *slog.Logger
}

//go:embed openapi.json
var openapi []byte

func New(repo Repository, log *slog.Logger) http.Handler {
	a := &API{repo: repo, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(openapi)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /api/agents", a.agents)
	mux.HandleFunc("GET /api/devices", a.devices)
	mux.HandleFunc("GET /api/devices/{id}", a.device)
	mux.HandleFunc("GET /api/devices/{id}/inventory", a.device)
	mux.HandleFunc("GET /api/devices/{id}/monitoring", a.monitoring)
	mux.HandleFunc("PUT /api/devices/{id}/monitoring", a.monitoring)
	mux.HandleFunc("GET /api/devices/{id}/metrics", a.metrics)
	mux.HandleFunc("GET /api/devices/{id}/telemetry", a.metrics)
	mux.HandleFunc("GET /api/devices/{id}/metrics/history", a.metrics)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func write(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = 500
		b = []byte(`{"error":{"code":"internal_error","message":"cannot encode response"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
func problem(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func (a *API) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		problem(w, 404, "not_found", "device or telemetry not found")
	case errors.Is(err, storage.ErrAmbiguous):
		problem(w, 409, "ambiguous_device", "specify agent_id query parameter")
	default:
		a.log.Error("API storage request failed", "error", err)
		problem(w, 503, "storage_unavailable", "storage temporarily unavailable")
	}
}
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if err := a.repo.Ping(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, map[string]string{"status": "ready"})
}
func (a *API) agents(w http.ResponseWriter, r *http.Request) {
	items, err := a.repo.Agents(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	if items == nil {
		items = []storage.Agent{}
	}
	write(w, 200, map[string]any{"items": items})
}
func number(r *http.Request, key string, fallback, min, max int) (int, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < min || n > max {
		return 0, errors.New("invalid pagination")
	}
	return n, nil
}
func (a *API) devices(w http.ResponseWriter, r *http.Request) {
	limit, err := number(r, "limit", 100, 1, 1000)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..1000")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "offset must be 0..1000000")
		return
	}
	items, err := a.repo.Devices(r.Context(), r.URL.Query().Get("agent_id"), "", limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	if items == nil {
		items = []storage.Device{}
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
func (a *API) device(w http.ResponseWriter, r *http.Request) {
	d, err := a.repo.Device(r.Context(), r.URL.Query().Get("agent_id"), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	if r.Pattern == "GET /api/devices/{id}/inventory" {
		write(w, 200, d.Inventory)
		return
	}
	write(w, 200, d)
}
func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	limit := 1
	history := r.Pattern == "GET /api/devices/{id}/metrics/history"
	if history {
		var err error
		limit, err = number(r, "limit", 100, 1, 1000)
		if err != nil {
			problem(w, 400, "invalid_query", "limit must be 1..1000")
			return
		}
	}
	d, err := a.repo.Device(r.Context(), r.URL.Query().Get("agent_id"), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	items, err := a.repo.Metrics(r.Context(), d.AgentID, d.DeviceID, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	if history {
		if items == nil {
			items = []storage.Telemetry{}
		}
		write(w, 200, map[string]any{"items": items, "limit": limit})
		return
	}
	if len(items) == 0 {
		a.fail(w, storage.ErrNotFound)
		return
	}
	t := items[0]
	write(w, 200, struct {
		storage.Telemetry
		Freshness  monitoring.Freshness `json:"freshness"`
		Monitoring monitoring.Settings  `json:"monitoring"`
	}{t, monitoring.Evaluate(time.Now(), &t.ObservedAt, &t.ReceivedAt, d.Monitoring.TelemetryIntervalSeconds), d.Monitoring})
}

func (a *API) monitoring(w http.ResponseWriter, r *http.Request) {
	d, err := a.repo.Device(r.Context(), r.URL.Query().Get("agent_id"), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	if r.Method == http.MethodGet {
		write(w, 200, d.Monitoring)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var p monitoring.Policy
	if err = decoder.Decode(&p); err != nil {
		problem(w, 400, "invalid_policy", "expected a JSON object with all three integer interval fields")
		return
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		problem(w, 400, "invalid_policy", "expected exactly one JSON object")
		return
	}
	if err = p.Validate(); err != nil {
		problem(w, 400, "invalid_policy", err.Error())
		return
	}
	if err = a.repo.UpdateMonitoring(r.Context(), d.AgentID, d.DeviceID, p); err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, monitoring.Describe(p, "device_override"))
}
