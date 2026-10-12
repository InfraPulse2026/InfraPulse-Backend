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

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
)

type Repository interface {
	Ping(context.Context) error
	Agents(context.Context) ([]storage.Agent, error)
	Devices(context.Context, string, string, int, int) ([]storage.Device, error)
	Device(context.Context, string, string) (storage.Device, error)
	Metrics(context.Context, string, string, int) ([]storage.Telemetry, error)
	UpdateMonitoring(context.Context, string, string, monitoring.Policy) error
	Topology(context.Context, string, string, bool, int, int) (topology.Graph, error)
	AvailabilitySummary(context.Context, string) (availability.Summary, error)
	AvailabilityChanges(context.Context, string, string, int, int) ([]availability.Change, error)
	Logs(context.Context, logbook.Filter) (logbook.Page, error)
	Log(context.Context, int64, string) (logbook.Record, error)
	LogsSummary(context.Context, logbook.Filter) (logbook.Summary, error)
	AuditEvents(context.Context, logbook.Filter) (storage.AuditPage, error)
	TelephonyTargets(context.Context, telephony.Filter) ([]telephony.Target, error)
	TelephonyTarget(context.Context, string, string) (telephony.Target, error)
	TelephonySummary(context.Context, string) (telephony.Summary, error)
	TelephonyHistory(context.Context, telephony.HistoryFilter) (telephony.HistoryPage, error)
	UpdateTelephonyMonitoring(context.Context, string, string, int) error
}
type API struct {
	remediation        RemediationRepository
	configurationDrift ConfigurationDriftRepository
	automation         AutomationRepository
	collectorConfig    CollectorConfigRepository
	hardware           AssetHardwareRepository
	assetLogs          AssetLogRepository
	voip               AssetVoIPRepository
	global             GlobalTopologyRepository
	physical           PhysicalRepository
	prometheus         PrometheusRepository
	external           ExternalMonitoringRepository
	assets             AssetRepository
	assetDecisions     AssetDecisionRepository
	dataSources        DataSourceRepository
	sourceJobs         DataSourceJobRepository
	repo               Repository
	log                *slog.Logger
	auth               *access.Service
	development        bool
	commands           CommandRepository
	pki                *pki.Service
	incidents          IncidentRepository
	reports            ReportRepository
	ai                 analysisai.Repository
	aiConfig           analysisai.Config
}

//go:embed openapi.json
var openapi []byte

func New(repo Repository, log *slog.Logger, options ...Option) http.Handler {
	a := &API{repo: repo, log: log}
	for _, option := range options {
		option(a)
	}
	mux := http.NewServeMux()
	register := func(pattern, permission string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, a.authorize(permission, handler))
	}
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/token", a.login)
	register("GET /api/auth/me", "", a.me)
	register("POST /api/auth/logout", "", a.logout)
	register("POST /api/auth/logout-all", "", a.logout)
	register("POST /api/auth/password", "", a.accessPassword)
	register("GET /api/access/users", access.AccessManage, a.accessUsers)
	register("POST /api/access/users", access.AccessManage, a.accessUsers)
	register("PATCH /api/access/users/{id}", access.AccessManage, a.accessUpdate)
	register("POST /api/access/users/{id}/password", access.AccessManage, a.accessPassword)
	register("GET /api/access/audit", access.AccessManage, a.accessAudit)
	register("GET /api/data-sources", access.DataRead, a.dataSourceRoutes)
	register("GET /api/data-sources/{id}", access.DataRead, a.dataSourceRoutes)
	register("GET /api/data-sources/{id}/status", access.DataRead, a.dataSourceRoutes)
	register("POST /api/data-sources", access.AccessManage, a.dataSourceRoutes)
	register("PUT /api/data-sources/{id}", access.AccessManage, a.dataSourceRoutes)
	register("POST /api/data-sources/{id}/test", access.AccessManage, a.dataSourceJobRoutes)
	register("POST /api/data-sources/{id}/sync", access.AccessManage, a.dataSourceJobRoutes)
	register("GET /api/data-sources/{id}/jobs", access.DataRead, a.dataSourceJobRoutes)
	register("GET /api/data-sources/{id}/jobs/{job_id}", access.DataRead, a.dataSourceJobRoutes)
	register("GET /api/data-sources/{id}/jobs/{job_id}/events", access.DataRead, a.dataSourceJobRoutes)
	register("GET /api/assets", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}/sources", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}/inventory", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}/facts", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}/identities", access.DataRead, a.assetRoutes)
	register("GET /api/assets/{id}/hardware", access.DataRead, a.assetHardwareRoutes)
	register("GET /api/assets/{id}/hardware/history", access.DataRead, a.assetHardwareRoutes)
	register("GET /api/assets/{id}/logs", access.DataRead, a.assetLogRoutes)
	register("GET /api/assets/{id}/logs/{log_id}", access.DataRead, a.assetLogRoutes)
	register("GET /api/assets/{id}/logs/{log_id}/raw", access.AccessManage, a.assetLogRoutes)
	register("GET /api/assets/{id}/audit/events", access.DataRead, a.assetLogRoutes)
	register("GET /api/assets/{id}/voip", access.DataRead, a.assetVoIPRoutes)
	register("POST /api/assets/{id}/voip", access.AccessManage, a.assetVoIPRoutes)
	register("PUT /api/assets/{id}/voip/{association_id}", access.AccessManage, a.assetVoIPRoutes)
	register("GET /api/assets/{id}/voip/{association_id}/history", access.DataRead, a.assetVoIPRoutes)
	register("GET /api/assets/{id}/voip/{association_id}/events", access.DataRead, a.assetVoIPRoutes)
	register("GET /api/assets/{id}/resolved-inventory", access.DataRead, a.physicalRoutes)
	register("GET /api/assets/{id}/placement", access.DataRead, a.physicalRoutes)
	register("PUT /api/assets/{id}/placement", access.AccessManage, a.physicalRoutes)
	register("GET /api/assets/{id}/placement/history", access.DataRead, a.physicalRoutes)
	register("POST /api/placements/import", access.AccessManage, a.physicalRoutes)
	register("GET /api/locations", access.DataRead, a.physicalRoutes)
	register("POST /api/locations", access.AccessManage, a.physicalRoutes)
	register("GET /api/locations/{id}", access.DataRead, a.physicalRoutes)
	register("PUT /api/locations/{id}", access.AccessManage, a.physicalRoutes)
	register("GET /api/locations/{id}/rack-layout", access.DataRead, a.physicalRoutes)
	register("GET /api/assets/{id}/candidates", access.DataRead, a.assetDecisionRoutes)
	register("GET /api/assets/{id}/decisions", access.DataRead, a.assetDecisionRoutes)
	register("GET /api/assets/{id}/resolution-events", access.DataRead, a.assetDecisionRoutes)
	register("GET /api/assets/{id}/external-metrics", access.DataRead, a.externalMonitoring)
	register("GET /api/assets/{id}/prometheus/instant", access.DataRead, a.prometheusQuery)
	register("GET /api/assets/{id}/prometheus/range", access.DataRead, a.prometheusQuery)
	register("GET /api/assets/{id}/prometheus/metadata", access.DataRead, a.prometheusQuery)
	register("GET /api/assets/{id}/signals", access.DataRead, a.externalMonitoring)
	register("GET /api/data-sources/{id}/metrics", access.DataRead, a.externalMonitoring)
	register("GET /api/data-sources/{id}/metrics/history", access.DataRead, a.externalMonitoring)
	register("GET /api/signals", access.DataRead, a.externalMonitoring)
	register("GET /api/data-sources/{id}/signals/{event_id}/history", access.DataRead, a.externalMonitoring)
	register("POST /api/assets/merge", access.AccessManage, a.assetDecisionRoutes)
	register("POST /api/assets/split", access.AccessManage, a.assetDecisionRoutes)
	register("GET /api/health/rules", access.DataRead, a.healthRoutes)
	register("GET /api/reports/summary", access.DataRead, a.reportRoutes)
	register("GET /api/reports/snapshots", access.DataRead, a.reportRoutes)
	register("POST /api/reports/snapshots", access.ReportWrite, a.reportRoutes)
	register("GET /api/reports/snapshots/{id}", access.DataRead, a.reportRoutes)
	register("GET /api/agents/{id}/health", access.DataRead, a.resourceHealthRoutes)
	register("GET /api/devices/{id}/interfaces/health", access.DataRead, a.resourceHealthRoutes)
	register("GET /api/devices/{id}/interfaces/{interface_id}/health", access.DataRead, a.resourceHealthRoutes)
	register("GET /api/telephony/targets/{id}/health", access.DataRead, a.resourceHealthRoutes)
	register("GET /api/devices/{id}/health", access.DataRead, a.healthRoutes)
	register("GET /api/incidents", access.DataRead, a.healthRoutes)
	register("GET /api/incidents/{id}", access.DataRead, a.healthRoutes)
	register("GET /api/incidents/{id}/evidence", access.DataRead, a.healthRoutes)
	register("GET /api/incidents/{id}/context", access.DataRead, a.incidentContext)
	register("POST /api/incidents/{id}/analysis/preview", access.DataRead, a.aiPreview)
	register("POST /api/incidents/{id}/analysis", access.AnalysisWrite, a.aiRoutes)
	register("GET /api/incidents/{id}/analyses", access.DataRead, a.aiRoutes)
	register("GET /api/analyses/{analysis_id}", access.DataRead, a.aiRoutes)
	register("GET /api/incidents/{id}/transitions", access.DataRead, a.healthRoutes)
	register("POST /api/incidents/{id}/acknowledge", access.IncidentWrite, a.healthRoutes)
	register("POST /api/pki/bootstrap-tokens", access.AccessManage, a.pkiManagement)
	register("GET /api/pki/bootstrap-tokens", access.AccessManage, a.pkiManagement)
	register("POST /api/pki/bootstrap-tokens/{id}/revoke", access.AccessManage, a.pkiManagement)
	register("GET /api/pki/certificates", access.AccessManage, a.pkiManagement)
	register("POST /api/pki/certificates/{id}/revoke", access.AccessManage, a.pkiManagement)
	register("GET /api/pki/audit", access.AccessManage, a.pkiManagement)
	register("GET /api/commands", access.DataRead, a.commandList)
	register("POST /api/commands", access.CommandWrite, a.createCommand)
	register("GET /api/commands/{id}", access.DataRead, a.command)
	register("GET /api/commands/{id}/events", access.DataRead, a.command)
	register("GET /api/commands/policy", access.DataRead, a.commandPolicy)
	register("POST /api/commands/{id}/rollback", access.CommandWrite, a.createRollback)
	register("GET /api/commands/{id}/snapshot", access.DataRead, a.commandSnapshot)
	register("GET /api/snapshots", access.DataRead, a.snapshotList)
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(openapi)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", a.ready)
	register("GET /api/automation/templates", access.DataRead, a.automationRoute)
	register("GET /api/assets/{id}/configuration/baseline", access.AccessManage, a.configurationDriftRoute)
	register("PUT /api/assets/{id}/configuration/baseline", access.AccessManage, a.configurationDriftRoute)
	register("GET /api/assets/{id}/configuration/baseline/history", access.AccessManage, a.configurationDriftRoute)
	register("GET /api/assets/{id}/configuration/drift", access.DataRead, a.configurationDriftRoute)
	register("GET /api/assets/{id}/configuration/drift/history", access.DataRead, a.configurationDriftRoute)
	register("POST /api/assets/{id}/configuration/compare", access.MonitoringWrite, a.configurationDriftRoute)
	register("POST /api/assets/{id}/configuration/remediations", access.CommandWrite, a.remediationRoute)
	register("GET /api/remediations/{id}", access.DataRead, a.remediationRoute)
	register("POST /api/remediations/{id}/approve", access.CommandWrite, a.remediationRoute)
	register("POST /api/remediations/{id}/reject", access.CommandWrite, a.remediationRoute)
	register("POST /api/remediations/{id}/reconcile", access.CommandWrite, a.remediationRoute)
	register("GET /api/remediations/{id}/history", access.DataRead, a.remediationRoute)
	register("GET /api/automation/workers", access.AccessManage, a.automationRoute)
	register("GET /api/assets/{id}/automation-target", access.AccessManage, a.automationRoute)
	register("PUT /api/assets/{id}/automation-target", access.AccessManage, a.automationRoute)
	register("POST /api/assets/{id}/automation/jobs", access.CommandWrite, a.automationRoute)
	register("GET /api/assets/{id}/automation/jobs", access.DataRead, a.automationRoute)
	register("GET /api/automation/jobs/{job_id}", access.DataRead, a.automationRoute)
	register("GET /api/automation/jobs/{job_id}/events", access.DataRead, a.automationRoute)
	register("POST /api/automation/jobs/{job_id}/cancel", access.CommandWrite, a.automationRoute)
	register("GET /api/assets/{id}/automation/snapshots", access.DataRead, a.automationRoute)
	register("GET /api/automation/snapshots/{snapshot_id}/content", access.AccessManage, a.automationRoute)
	register("GET /api/collectors/{id}/config", access.AccessManage, a.collectorConfigRoute)
	register("PUT /api/collectors/{id}/config", access.AccessManage, a.collectorConfigRoute)
	register("GET /api/collectors/{id}/config/history", access.AccessManage, a.collectorConfigRoute)
	register("GET /api/collectors/{id}/config/events", access.AccessManage, a.collectorConfigRoute)
	register("GET /api/agents", access.DataRead, a.agents)
	register("GET /api/availability/summary", access.DataRead, a.availabilitySummary)
	register("GET /api/telephony/targets", access.DataRead, a.sipTargets)
	register("GET /api/telephony/targets/{id}", access.DataRead, a.sipTarget)
	register("GET /api/telephony/targets/{id}/history", access.DataRead, a.sipHistory)
	register("GET /api/telephony/targets/{id}/monitoring", access.DataRead, a.sipMonitoring)
	register("PUT /api/telephony/targets/{id}/monitoring", access.MonitoringWrite, a.sipMonitoring)
	register("GET /api/telephony/summary", access.DataRead, a.sipSummary)
	register("GET /api/logs", access.DataRead, a.logs)
	register("GET /api/logs/summary", access.DataRead, a.logsSummary)
	register("GET /api/logs/{id}", access.DataRead, a.logRecord)
	register("GET /api/logs/{id}/raw", access.AccessManage, a.legacyLogRaw)
	register("GET /api/audit", access.DataRead, a.logs)
	register("GET /api/audit/events", access.DataRead, a.auditEvents)
	register("GET /api/audit/events/{key}", access.DataRead, a.auditEvent)
	register("GET /api/devices/{id}/logs", access.DataRead, a.logs)
	register("GET /api/devices/{id}/audit", access.DataRead, a.logs)
	register("GET /api/devices", access.DataRead, a.devices)
	register("GET /api/topology", access.DataRead, a.topology)
	register("GET /api/topology/global", access.DataRead, a.globalTopology)
	register("GET /api/topology/changes", access.DataRead, a.globalTopology)
	register("GET /api/assets/{id}/neighbors", access.DataRead, a.globalTopology)
	register("GET /api/devices/{id}/topology", access.DataRead, a.topology)
	register("GET /api/devices/{id}", access.DataRead, a.device)
	register("GET /api/devices/{id}/availability", access.DataRead, a.deviceAvailability)
	register("GET /api/devices/{id}/availability/history", access.DataRead, a.availabilityHistory)
	register("GET /api/devices/{id}/inventory", access.DataRead, a.device)
	register("GET /api/devices/{id}/monitoring", access.DataRead, a.monitoring)
	register("PUT /api/devices/{id}/monitoring", access.MonitoringWrite, a.monitoring)
	register("GET /api/devices/{id}/metrics", access.DataRead, a.metrics)
	register("GET /api/devices/{id}/telemetry", access.DataRead, a.metrics)
	register("GET /api/devices/{id}/metrics/history", access.DataRead, a.metrics)
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
	case errors.Is(err, access.ErrForbidden) || errors.Is(err, access.ErrCredentials):
		a.accessFail(w, err)
	case errors.Is(err, storage.ErrNotFound):
		problem(w, 404, "not_found", "requested resource not found")
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
