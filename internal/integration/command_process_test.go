package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

const commandFixturePassword = "private-command-fixture-password"

// commandRouterFixture implements the fixed RouterOS endpoints used by the
// unmodified P3 Agent. It binds only to loopback and has no hardware connection.
type commandRouterFixture struct {
	disabled         atomic.Bool
	writes           atomic.Int32
	unexpectedWrites atomic.Int32
	counter          atomic.Uint64
}

func (f *commandRouterFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != "monitor" || p != commandFixturePassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && !(r.Method == http.MethodPatch && r.URL.Path == "/rest/interface/*2") {
		f.unexpectedWrites.Add(1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	record := func(id, name string) map[string]string {
		disabled := id == "*2" && f.disabled.Load()
		n := f.counter.Add(100)
		return map[string]string{
			".id": id, "name": name, "default-name": name, "type": "ether",
			"mac-address": "02:00:00:00:00:0" + id[1:], "mtu": "1500",
			"disabled": strconv.FormatBool(disabled), "running": strconv.FormatBool(!disabled),
			"dynamic": "false", "rx-byte": strconv.FormatUint(n, 10), "tx-byte": strconv.FormatUint(n*2, 10),
			"rx-packet": "10", "tx-packet": "20", "rx-error": "0", "tx-error": "0", "rx-drop": "0", "tx-drop": "0",
		}
	}
	var data any
	switch r.URL.Path {
	case "/rest/system/identity":
		data = map[string]string{"name": "fixture-router"}
	case "/rest/system/resource":
		data = map[string]string{"uptime": "1h2m3s", "version": "7.20", "architecture-name": "arm64", "board-name": "fixture-board", "cpu-load": "5", "free-memory": "4096", "total-memory": "8192"}
	case "/rest/system/routerboard":
		data = map[string]string{"model": "fixture-board", "serial-number": "fixture-serial", "current-firmware": "7.20"}
	case "/rest/interface":
		data = []map[string]string{record("*1", "ether1"), record("*2", "ether2")}
	case "/rest/interface/*1":
		data = record("*1", "ether1")
	case "/rest/interface/*2":
		if r.Method == http.MethodPatch {
			var body struct {
				Disabled *bool `json:"disabled"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil || body.Disabled == nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.disabled.Store(*body.Disabled)
			f.writes.Add(1)
		}
		data = record("*2", "ether2")
	case "/rest/ip/address":
		data = []map[string]string{{"address": "192.0.2.1/24", "interface": "ether1", "disabled": "false"}}
	case "/rest/ipv6/address", "/rest/interface/bridge/port", "/rest/interface/bonding", "/rest/interface/vlan":
		data = []map[string]string{}
	case "/rest/ip/neighbor":
		data = []map[string]string{{"interface": "ether1", "identity": "unknown-switch", "mac-address": "02:10:00:00:00:01", "discovered-by": "lldp", "age": "1s"}}
	case "/rest/user":
		data = []map[string]string{{"name": "monitor", "group": "fixture-write", "disabled": "false"}}
	case "/rest/user/group":
		data = []map[string]string{{"name": "fixture-write", "policy": "read,write,api,rest-api"}}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(data)
}

// Opt in with both AGENT_BINARY pointing to a separately built unmodified P3
// Agent executable and TEST_DATABASE_URL pointing to a disposable loopback
// PostgreSQL database. Agent source is never built or edited here; this fixture
// test makes no production or hardware-validation claim.
func TestUnmodifiedAgentRouterOSCommandsThroughAuthenticatedREST(t *testing.T) {
	runAgentCommandScenario(t, false)
}
func TestUnmodifiedAgentRouterOSRollbackThroughAuthenticatedREST(t *testing.T) {
	runAgentCommandScenario(t, true)
}
func runAgentCommandScenario(t *testing.T, withRollback bool) {
	binary, dbURL := os.Getenv("AGENT_BINARY"), os.Getenv("TEST_DATABASE_URL")
	if binary == "" || dbURL == "" {
		t.Skip("set AGENT_BINARY and TEST_DATABASE_URL for the real Agent command process integration test")
	}
	parsed, err := url.Parse(dbURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL for a disposable loopback database")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("command process integration is restricted to loopback PostgreSQL")
	}
	// pgx URL parameters can override the URL host; validate the effective host
	// too, so an inherited query cannot route this local fixture to an external DB.
	dbConfig, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	if ip := net.ParseIP(dbConfig.Host); !strings.EqualFold(dbConfig.Host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("effective PostgreSQL host must be loopback")
	}
	for _, fallback := range dbConfig.Fallbacks {
		ip := net.ParseIP(fallback.Host)
		if !strings.EqualFold(fallback.Host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			t.Fatal("PostgreSQL fallback hosts must be loopback")
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatal("Agent executable is unavailable; set AGENT_BINARY to the unmodified P3 Agent")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("infrapulse_command_process_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up command process schema: %v", err)
		}
	}()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := storage.Open(ctx, parsed.String(), 10*time.Second, monitoring.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store.ConfigureCommands(commands.Policy{Enabled: true, AllowDisable: true, AllowRollback: withRollback, ProtectedInterfaces: []string{"ether1"}})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := controller.New(store, logger)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterAgentServiceServer(g, c)
	grpcDone := make(chan error, 1)
	go func() { grpcDone <- g.Serve(listener) }()
	defer func() {
		g.Stop()
		if err := <-grpcDone; err != nil {
			t.Errorf("gRPC fixture server: %v", err)
		}
	}()
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		commands.Run(workerCtx, store, c, logger)
	}()
	defer func() {
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("command worker did not stop")
		}
	}()

	const password = "command-process-fixture-admin-password"
	hash, err := access.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AccessCreateUser(ctx, access.Principal{}, "command-process-admin", "admin", hash, true, access.Meta{Peer: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	auth := access.New(store, access.Config{Mode: "required", SessionTTL: time.Hour})
	rest := httptest.NewServer(api.New(store, logger, api.WithAccess(auth), api.WithCommands(store)))
	defer rest.Close()
	client := rest.Client()
	client.Timeout = 6 * time.Second
	token := "" // The raw bearer token stays in memory and is never written to proof/log files.
	request := func(method, path, key string, payload any) (int, http.Header, []byte) {
		t.Helper()
		var body io.Reader
		if payload != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, rest.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header.Clone(), data
	}
	status, _, data := request(http.MethodGet, "/api/commands", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated command read returned %d, want 401", status)
	}
	status, _, data = request(http.MethodPost, "/api/auth/token", "", map[string]string{"username": "command-process-admin", "password": password})
	var login struct {
		Token string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(data, &login) != nil || login.Token == "" {
		t.Fatalf("fixture admin login failed (HTTP %d)", status)
	}
	token = login.Token

	fixture := &commandRouterFixture{}
	router := httptest.NewTLSServer(fixture)
	defer router.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "router-ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: router.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "agent.yaml")
	config := fmt.Sprintf(`agent:
  name: command-process-fixture
  state_dir: state
controller:
  address: %s
  connect_timeout: 1s
  reconnect_initial: 100ms
  reconnect_max: 1s
security:
  mode: development
local:
  enabled: false
commands:
  enabled: true
  protected_interfaces: [ether1]
queue:
  mode: durable
  ack_fallback: reject
telemetry:
  fast_interval: 500ms
  medium_interval: 1s
  inventory_interval: 1s
  topology_interval: 1s
  heartbeat_interval: 1s
  collect_timeout: 1s
credentials:
  router:
    username_env: COMMAND_PROCESS_ROUTER_USER
    password_env: COMMAND_PROCESS_ROUTER_PASSWORD
devices:
  - id: fixture-router
    name: fixture-router
    driver: routeros
    address: %s
    credential_ref: router
    enable_mutations: true
    tls:
      ca_file: router-ca.pem
    polling:
      fast_interval: 500ms
      medium_interval: 1s
      inventory_interval: 1s
      topology_interval: 1s
      timeout: 1s
`, listener.Addr(), router.URL)
	if err = os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "agent-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	process := exec.CommandContext(ctx, binary, "run", "--config", configPath)
	process.Dir = dir
	process.Stdout, process.Stderr = output, output
	// Config path overrides must not redirect the Agent's fresh state or TLS
	// files to any inherited location outside this test's temporary directory.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "INFRAPULSE_STATE_DIR", "INFRAPULSE_CA_FILE", "INFRAPULSE_CERT_FILE", "INFRAPULSE_KEY_FILE", "COMMAND_PROCESS_ROUTER_USER", "COMMAND_PROCESS_ROUTER_PASSWORD":
			continue
		}
		process.Env = append(process.Env, value)
	}
	process.Env = append(process.Env, "COMMAND_PROCESS_ROUTER_USER=monitor", "COMMAND_PROCESS_ROUTER_PASSWORD="+commandFixturePassword)
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = process.Wait()
		close(processDone)
	}()
	defer func() {
		cancel()
		select {
		case <-processDone:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
		if t.Failed() {
			if log, err := os.ReadFile(output.Name()); err == nil {
				text := strings.NewReplacer(commandFixturePassword, "[redacted]", password, "[redacted]", token, "[redacted]").Replace(string(log))
				if len(text) > 12000 {
					text = text[len(text)-12000:]
				}
				t.Logf("Agent output (sanitized):\n%s", text)
			}
		}
	}()
	wait := func(description string, predicate func() (bool, string)) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		last := "no observation"
		for time.Now().Before(deadline) {
			select {
			case <-processDone:
				t.Fatalf("Agent exited while waiting for %s: %v", description, processErr)
			default:
			}
			if ready, detail := predicate(); ready {
				return
			} else {
				last = detail
			}
			select {
			case <-processDone:
				t.Fatalf("Agent exited while waiting for %s: %v", description, processErr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s: %s", description, last)
	}
	agentID := ""
	waitInterface := func(wantUp bool, after time.Time) {
		t.Helper()
		wait(fmt.Sprintf("fresh remote telemetry with ether2 admin_up=%t", wantUp), func() (bool, string) {
			status, _, data := request(http.MethodGet, "/api/devices", "", nil)
			if status != http.StatusOK {
				t.Fatalf("device discovery returned HTTP %d: %s", status, data)
			}
			var list struct {
				Items []storage.Device `json:"items"`
			}
			if err := json.Unmarshal(data, &list); err != nil {
				t.Fatal(err)
			}
			var device *storage.Device
			for i := range list.Items {
				if list.Items[i].DeviceID == "fixture-router" {
					device = &list.Items[i]
					break
				}
			}
			if device == nil || !device.Online || !device.AgentConnected {
				return false, "remote device not yet online with connected Agent"
			}
			inventory := &pb.Device{}
			if err := protojson.Unmarshal(device.Inventory, inventory); err != nil {
				t.Fatalf("decode remote inventory: %v", err)
			}
			if inventory.Driver != "routeros-rest" || !inventory.Online || inventory.LastSeen == nil || time.Since(inventory.LastSeen.AsTime()) > 5*time.Minute {
				return false, "fresh RouterOS REST inventory not observed"
			}
			caps := map[string]bool{}
			for _, capability := range inventory.Capabilities {
				caps[capability.Name] = capability.Supported
			}
			if !caps["interfaces.enable"] || !caps["interfaces.disable"] {
				return false, "Agent has not yet reported both mutation capabilities"
			}
			if agentID != "" && agentID != device.AgentID {
				t.Fatal("fixture Agent identity changed")
			}
			agentID = device.AgentID
			status, _, data = request(http.MethodGet, "/api/devices/fixture-router/telemetry?agent_id="+url.QueryEscape(agentID), "", nil)
			if status == http.StatusNotFound {
				return false, "no remote telemetry yet"
			}
			if status != http.StatusOK {
				t.Fatalf("remote telemetry returned HTTP %d: %s", status, data)
			}
			var metric storage.Telemetry
			if err := json.Unmarshal(data, &metric); err != nil {
				t.Fatal(err)
			}
			if !metric.ObservedAt.After(after) || time.Since(metric.ObservedAt) > 5*time.Second {
				return false, "waiting for a newer fresh telemetry poll"
			}
			sample := &pb.TelemetrySample{}
			if err := protojson.Unmarshal(metric.Sample, sample); err != nil {
				t.Fatalf("decode remote telemetry: %v", err)
			}
			if metric.Source != "routeros-rest" || sample.Source != "routeros-rest" {
				t.Fatal("fixture telemetry lost its RouterOS REST source identity")
			}
			for _, iface := range sample.Interfaces {
				if iface.Id == "*2" && iface.Name == "ether2" && iface.Metadata.GetAdminStatus() && iface.AdminUp == wantUp {
					return true, ""
				}
			}
			return false, "ether2 expected state with explicit admin-status support not observed"
		})
	}
	waitInterface(true, time.Time{})
	if fixture.writes.Load() != 0 || fixture.unexpectedWrites.Load() != 0 {
		t.Fatal("collection unexpectedly attempted a mutation")
	}

	create := func(key, iface, operation string, expected bool, wantStatus int) (commands.Record, http.Header) {
		t.Helper()
		payload := commands.Request{AgentID: agentID, DeviceID: "fixture-router", InterfaceID: iface, Operation: operation, ExpectedAdminUp: &expected, TimeoutSeconds: 30, Confirmed: true}
		status, headers, data := request(http.MethodPost, "/api/commands", key, payload)
		if status != wantStatus {
			t.Fatalf("%s %s returned HTTP %d, want %d: %s", operation, iface, status, wantStatus, data)
		}
		var record commands.Record
		if wantStatus == http.StatusForbidden {
			var problem struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &problem) != nil || problem.Error.Code != "protected_resource" {
				t.Fatalf("protected request did not report protected_resource: %s", data)
			}
			return record, headers
		}
		if err := json.Unmarshal(data, &record); err != nil || record.ID == "" {
			t.Fatalf("invalid command response: %s", data)
		}
		return record, headers
	}
	waitSuccess := func(id string, wantUp bool) commands.Record {
		t.Helper()
		var result commands.Record
		wait("verified command "+id, func() (bool, string) {
			status, _, data := request(http.MethodGet, "/api/commands/"+id, "", nil)
			if status != http.StatusOK || json.Unmarshal(data, &result) != nil {
				t.Fatalf("command read failed (HTTP %d): %s", status, data)
			}
			switch result.State {
			case "queued", "dispatching", "sent", "accepted", "running":
				return false, result.State
			case "success":
			default:
				t.Fatalf("command ended without success: %s", data)
			}
			v := result.Result
			if v == nil || v.Status != "success" || !v.Changed || v.Uncertain || v.SnapshotID == "" || v.StartedAt == nil || v.FinishedAt == nil || v.StartedAt.IsZero() || v.FinishedAt.IsZero() || v.FinishedAt.Before(*v.StartedAt) || v.Verification == nil || !v.Verification.Verified || v.Verification.Uncertain || v.Verification.ObservedAt == nil || v.Verification.ObservedAt.IsZero() || v.Verification.ObservedAdminUp == nil || *v.Verification.ObservedAdminUp != wantUp || v.Verification.ExpectedAdminUp != wantUp || v.Verification.InterfaceID != "*2" {
				t.Fatalf("successful command lacks the expected verified result and snapshot: %s", data)
			}
			return true, ""
		})
		return result
	}
	disable, _ := create("fixture-disable-once", "ether2", "disable", true, http.StatusAccepted)
	disable = waitSuccess(disable.ID, false)
	if !fixture.disabled.Load() || fixture.writes.Load() != 1 || fixture.unexpectedWrites.Load() != 0 {
		t.Fatalf("disable fixture state: disabled=%t writes=%d unexpected=%d", fixture.disabled.Load(), fixture.writes.Load(), fixture.unexpectedWrites.Load())
	}
	replayed, headers := create("fixture-disable-once", "ether2", "disable", true, http.StatusOK)
	if replayed.ID != disable.ID || replayed.State != "success" || headers.Get("Idempotent-Replay") != "true" {
		t.Fatalf("idempotent REST replay changed command identity/state: %+v", replayed)
	}
	waitInterface(false, *disable.Result.FinishedAt)
	if fixture.writes.Load() != 1 || fixture.unexpectedWrites.Load() != 0 {
		t.Fatal("idempotent replay issued a second RouterOS write")
	}
	enable, _ := create("fixture-enable-once", "ether2", "enable", false, http.StatusAccepted)
	enable = waitSuccess(enable.ID, true)
	if fixture.disabled.Load() || fixture.writes.Load() != 2 || fixture.unexpectedWrites.Load() != 0 {
		t.Fatalf("enable fixture state: disabled=%t writes=%d unexpected=%d", fixture.disabled.Load(), fixture.writes.Load(), fixture.unexpectedWrites.Load())
	}
	waitInterface(true, *enable.Result.FinishedAt)
	_, _ = create("fixture-protected", "ether1", "disable", true, http.StatusForbidden)
	// Wait across worker cycles and a real Agent poll before checking rejection
	// caused no asynchronous command or write, rather than only checking HTTP.
	waitInterface(true, time.Now().UTC())
	if fixture.writes.Load() != 2 || fixture.unexpectedWrites.Load() != 0 {
		t.Fatal("protected ether1 request reached the RouterOS mutation fixture")
	}
	status, _, data = request(http.MethodGet, "/api/commands?agent_id="+url.QueryEscape(agentID), "", nil)
	var commandList struct {
		Items []commands.Record `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(data, &commandList) != nil || len(commandList.Items) != 2 {
		t.Fatalf("expected only the two persisted successful commands (HTTP %d): %s", status, data)
	}
	for _, record := range commandList.Items {
		if record.State != "success" || record.ActorID == "" || record.SessionID == "" || record.InterfaceID != "*2" {
			t.Fatalf("persisted command identity/result incomplete: %+v", record)
		}
	}
	t.Log("loopback fixture verified disable, REST idempotency replay, enable, and protected rejection; exactly two RouterOS writes")
	if !withRollback {
		if proofDir := os.Getenv("COMMAND_PROOF_DIR"); proofDir != "" {
			proof := map[string]any{"data_source": "local_routeros_https_fixture", "agent": "unmodified P3", "writes": fixture.writes.Load(), "disable": disable, "enable": enable, "idempotent_replay_id": replayed.ID, "protected_http_status": http.StatusForbidden}
			encoded, err := json.MarshalIndent(proof, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(proofDir, "commands.fixture.json"), append(encoded, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	baseline, _ := create("fixture-disable-before-rollback", "ether2", "disable", true, http.StatusAccepted)
	baseline = waitSuccess(baseline.ID, false)
	waitInterface(false, *baseline.Result.FinishedAt)
	status, _, data = request(http.MethodGet, "/api/commands/"+baseline.ID+"/snapshot", "", nil)
	var snapshot commands.Snapshot
	if status != 200 || json.Unmarshal(data, &snapshot) != nil || snapshot.Status != "candidate" || snapshot.ExpiresAt != nil || snapshot.SnapshotID != baseline.Result.SnapshotID {
		t.Fatal("Agent snapshot reference incorrectly described")
	}
	expected := false
	rollbackBody := commands.RollbackRequest{ExpectedAdminUp: &expected, TimeoutSeconds: 30, Confirmed: true}
	status, _, data = request(http.MethodPost, "/api/commands/"+baseline.ID+"/rollback", "fixture-real-rollback", rollbackBody)
	var undo commands.Record
	if status != 202 || json.Unmarshal(data, &undo) != nil || undo.Operation != "rollback" || undo.RollbackSnapshotID == nil || *undo.RollbackSnapshotID != snapshot.SnapshotID {
		t.Fatalf("typed rollback was not created: %d %s", status, data)
	}
	undo = waitSuccess(undo.ID, true)
	waitInterface(true, *undo.Result.FinishedAt)
	if fixture.disabled.Load() || fixture.writes.Load() != 4 {
		t.Fatal("real RollbackAction did not restore fixture state exactly once")
	}
	status, headers, data = request(http.MethodPost, "/api/commands/"+baseline.ID+"/rollback", "fixture-real-rollback", rollbackBody)
	var replay commands.Record
	if status != 200 || json.Unmarshal(data, &replay) != nil || replay.ID != undo.ID || headers.Get("Idempotent-Replay") != "true" {
		t.Fatal("rollback repeat created another command")
	}
	status, _, data = request(http.MethodGet, "/api/commands/"+baseline.ID+"/snapshot", "", nil)
	if status != 200 || json.Unmarshal(data, &snapshot) != nil || snapshot.Status != "consumed" {
		t.Fatal("successful rollback did not consume source snapshot")
	}
	status, _, _ = request(http.MethodPost, "/api/commands/"+baseline.ID+"/rollback", "consume-again", rollbackBody)
	if status != 409 || fixture.writes.Load() != 4 {
		t.Fatal("consumed snapshot issued another mutation")
	}
	t.Log("real Agent RollbackAction restored its owned snapshot; replay no additional PATCH, consumed snapshot blocked")
	if proofDir := os.Getenv("COMMAND_PROOF_DIR"); proofDir != "" {
		proof := map[string]any{"data_source": "local_routeros_https_fixture", "agent": "unmodified P3", "writes": fixture.writes.Load(), "source_command": baseline, "rollback": undo, "snapshot_after_rollback": snapshot, "consumed_retry_http_status": status}
		encoded, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(proofDir, "rollback.fixture.json"), append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
