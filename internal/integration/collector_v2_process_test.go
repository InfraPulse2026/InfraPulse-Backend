package integration

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
)

// Commit a log once, then lose the ACK. Real Collector must replay the same
// logical message through a new stream without duplicating searchable records.
type ackLossRepository struct {
	*storage.Store
	lost        atomic.Bool
	logAttempts atomic.Int32
	lastError   atomic.Value
}

func (r *ackLossRepository) Ingest(ctx context.Context, e *pb.AgentEnvelope) error {
	if e.GetLogs() != nil {
		r.logAttempts.Add(1)
	}
	if err := r.Store.Ingest(ctx, e); err != nil {
		r.lastError.Store(err.Error())
		return err
	}
	if e.GetLogs() != nil && r.lost.CompareAndSwap(false, true) {
		return errors.New("fixture: ACK lost after COMMIT")
	}
	return nil
}

func TestUnmodifiedCollectorV2RedfishSyslogReplayAndREST(t *testing.T) {
	binary, db := os.Getenv("COLLECTOR_V2_BINARY"), os.Getenv("TEST_DATABASE_URL")
	if binary == "" || db == "" {
		t.Skip("set COLLECTOR_V2_BINARY and loopback TEST_DATABASE_URL")
	}
	parsed, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("requires disposable loopback PostgreSQL")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("infrapulse_collector_v2_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") }()
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := storage.Open(ctx, parsed.String(), 15*time.Second, monitoring.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := &ackLossRepository{Store: store}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterAgentServiceServer(g, controller.New(repo, logger))
	go g.Serve(listener)
	defer func() { g.Stop() }()
	// Synthetic read-only BMC, with certificate verification by the actual adapter.
	link := func(p string) map[string]string { return map[string]string{"@odata.id": p} }
	collection := func(kind, path, member string) map[string]any {
		return map[string]any{"@odata.id": path, "@odata.type": "#" + kind + "Collection." + kind + "Collection", "Name": kind, "Members": []any{link(member)}, "Members@odata.count": 1}
	}
	resources := map[string]any{
		"/redfish/v1":                   map[string]any{"@odata.id": "/redfish/v1/", "@odata.type": "#ServiceRoot.v1_16_0.ServiceRoot", "RedfishVersion": "1.16.0", "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "Managers": link("/redfish/v1/Managers")},
		"/redfish/v1/Managers":          collection("Manager", "/redfish/v1/Managers", "/redfish/v1/Managers/1"),
		"/redfish/v1/Managers/1":        map[string]any{"@odata.id": "/redfish/v1/Managers/1", "@odata.type": "#Manager.v1_20_0.Manager", "Id": "1", "Name": "fixture-bmc", "ManagerType": "BMC", "Status": map[string]string{"Health": "OK", "State": "Enabled"}, "Links": map[string]any{"ManagerForServers": []any{link("/redfish/v1/Systems/1")}}},
		"/redfish/v1/Systems":           collection("ComputerSystem", "/redfish/v1/Systems", "/redfish/v1/Systems/1"),
		"/redfish/v1/Systems/1":         map[string]any{"@odata.id": "/redfish/v1/Systems/1", "@odata.type": "#ComputerSystem.v1_22_0.ComputerSystem", "Id": "1", "Name": "fixture-system", "Manufacturer": "Fixture Equipment", "SerialNumber": "FIXTURE-SERIAL-1", "UUID": "38947555-7742-3448-3784-823347823834", "PowerState": "Off", "Status": map[string]string{"Health": "OK", "HealthRollup": "OK", "State": "Enabled"}, "Links": map[string]any{"Chassis": []any{link("/redfish/v1/Chassis/1")}}},
		"/redfish/v1/Chassis":           collection("Chassis", "/redfish/v1/Chassis", "/redfish/v1/Chassis/1"),
		"/redfish/v1/Chassis/1":         map[string]any{"@odata.id": "/redfish/v1/Chassis/1", "@odata.type": "#Chassis.v1_30_0.Chassis", "Id": "1", "Name": "fixture-chassis", "ChassisType": "RackMount", "Status": map[string]string{"Health": "Warning", "State": "Enabled"}, "Thermal": link("/redfish/v1/Chassis/1/Thermal")},
		"/redfish/v1/Chassis/1/Thermal": map[string]any{"@odata.id": "/redfish/v1/Chassis/1/Thermal", "@odata.type": "#Thermal.v1_7_0.Thermal", "Id": "Thermal", "Temperatures": []any{map[string]any{"MemberId": "CPU", "Name": "CPU Temperature", "ReadingCelsius": 42, "Status": map[string]string{"Health": "OK", "State": "Enabled"}}}, "Fans": []any{map[string]any{"MemberId": "FAN", "Name": "Fan", "Reading": 4200, "ReadingUnits": "RPM", "Status": map[string]string{"Health": "Warning", "State": "Enabled"}}}},
	}
	var requests, unauthorized atomic.Int32
	bmc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" {
			t.Error("BMC mutation attempted")
			w.WriteHeader(405)
			return
		}
		user, password, ok := r.BasicAuth()
		// Redfish service discovery precedes Basic authentication in gofish.
		// Only the service root is public; every hardware resource requires auth.
		if strings.TrimSuffix(r.URL.Path, "/") != "/redfish/v1" && (!ok || user != "fixture-monitor" || password != "fixture-bmc-password") {
			unauthorized.Add(1)
			w.WriteHeader(401)
			return
		}
		body, ok := resources[strings.TrimSuffix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer bmc.Close()
	defer func() {
		if t.Failed() {
			t.Logf("BMC requests=%d unauthorized=%d storage_error=%v", requests.Load(), unauthorized.Load(), repo.lastError.Load())
			var inventory, samples int
			_ = admin.QueryRow(context.Background(), "SELECT count(*) FROM "+quoted+".devices WHERE inventory ? 'hardware'").Scan(&inventory)
			_ = admin.QueryRow(context.Background(), "SELECT count(*) FROM "+quoted+".telemetry").Scan(&samples)
			t.Logf("hardware inventories=%d all telemetry=%d", inventory, samples)
		}
	}()
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "bmc-ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bmc.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	syslogListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	syslogAddress := syslogListener.Addr().String()
	syslogListener.Close()
	config := fmt.Sprintf(`agent:
  name: collector-v2-fixture
  state_dir: state
controller:
  address: %s
  reconnect_initial: 100ms
  reconnect_max: 500ms
security:
  mode: development
local:
  enabled: false
queue:
  mode: durable
  ack_fallback: reject
telemetry:
  heartbeat_interval: 1s
commands:
  enabled: false
credentials:
  bmc:
    username_env: COLLECTOR_FIXTURE_USER
    password_env: COLLECTOR_FIXTURE_PASSWORD
devices:
  - id: bmc-fixture
    driver: redfish
    address: %s
    credential_ref: bmc
    tls:
      ca_file: bmc-ca.pem
    polling:
      medium_interval: 1s
      inventory_interval: 1s
      timeout: 5s
    redfish:
      auth: basic
      provenance: simulated
syslog_receiver:
  enabled: true
  unknown_source_policy: reject
  tls:
    enabled: false
  tcp:
    enabled: true
    listen: %s
    framing: octet_counting
  sources:
    - id: linux-fixture
      peer_cidrs: ["127.0.0.1/32"]
      provenance: simulated
`, listener.Addr(), bmc.URL, syslogAddress)
	configPath := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "collector-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	process := exec.CommandContext(ctx, binary, "run", "--config", configPath)
	process.Dir = dir
	process.Stdout = output
	process.Stderr = output
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "INFRAPULSE_STATE_DIR", "INFRAPULSE_CA_FILE", "INFRAPULSE_CERT_FILE", "INFRAPULSE_KEY_FILE", "COLLECTOR_FIXTURE_USER", "COLLECTOR_FIXTURE_PASSWORD":
			continue
		}
		process.Env = append(process.Env, value)
	}
	process.Env = append(process.Env, "COLLECTOR_FIXTURE_USER=fixture-monitor", "COLLECTOR_FIXTURE_PASSWORD=fixture-bmc-password")
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Collector did not stop")
		}
		if t.Failed() {
			b, _ := os.ReadFile(output.Name())
			text := strings.ReplaceAll(string(b), "fixture-bmc-password", "[redacted]")
			if len(text) > 9000 {
				text = text[len(text)-9000:]
			}
			t.Log(text)
		}
	}()
	wait := func(label string, condition func() bool) {
		t.Helper()
		for {
			if condition() {
				return
			}
			select {
			case processErr := <-done:
				done <- processErr
				t.Fatalf("Collector exited while waiting for %s: %v", label, processErr)
			case <-ctx.Done():
				t.Fatal("timeout: " + label)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	var conn net.Conn
	wait("syslog listener", func() bool {
		conn, err = net.DialTimeout("tcp", syslogAddress, 100*time.Millisecond)
		return err == nil
	})
	defer conn.Close()
	msg := "<14>1 2026-10-10T00:00:00Z claimed-host fixture 123 B1 - collector-v2-proof password=fixture-log-secret"
	if _, err = fmt.Fprintf(conn, "%d %s", len(msg), msg); err != nil {
		t.Fatal(err)
	}
	wait("durable replay after lost ACK", func() bool { return repo.logAttempts.Load() >= 2 })
	// Keep Collector/syslog alive while the backend listener is unavailable.
	backendAddress := listener.Addr().String()
	g.Stop()
	msg = "<14>1 2026-10-10T00:00:01Z claimed-host fixture 123 B1 - collector-v2-offline-proof"
	if _, err = fmt.Fprintf(conn, "%d %s", len(msg), msg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(time.Second):
	}
	var offlineCount int
	if err = admin.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".log_records").Scan(&offlineCount); err != nil || offlineCount != 1 {
		t.Fatalf("data delivered while backend offline: %d %v", offlineCount, err)
	}
	listener, err = net.Listen("tcp", backendAddress)
	if err != nil {
		t.Fatal(err)
	}
	g = grpc.NewServer()
	pb.RegisterAgentServiceServer(g, controller.New(repo, logger))
	go g.Serve(listener)
	wait("outbox delivery after backend reconnect", func() bool {
		var count int
		return admin.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".log_records").Scan(&count) == nil && count == 2
	})
	var agentID, deviceID string
	wait("Redfish inventory and metrics", func() bool {
		var inventory, samples int
		err = admin.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".devices WHERE inventory ? 'hardware'").Scan(&inventory)
		if err != nil {
			return false
		}
		err = admin.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".telemetry WHERE payload ? 'hardware_health' AND payload ? 'metrics'").Scan(&samples)
		return err == nil && inventory >= 2 && samples > 0
	})
	if err = admin.QueryRow(ctx, "SELECT agent_id,device_id FROM "+quoted+".devices WHERE inventory->'hardware'->>'power_state'='Off' LIMIT 1").Scan(&agentID, &deviceID); err != nil {
		t.Fatal(err)
	}
	page, err := store.Logs(ctx, logbook.Filter{AgentID: agentID, Limit: 10})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("replayed logs duplicated: %v %d", err, len(page.Items))
	}
	if page.Items[0].Syslog == nil || page.Items[0].Observation == nil || strings.Contains(page.Items[0].Message, "fixture-log-secret") {
		t.Fatal("syslog metadata or redaction failed")
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal("repeat migrations", err)
	} // one Controller fixture only
	// Authenticate REST exactly as a client would; no development auth bypass.
	hash, err := access.HashPassword("fixture-backend-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AccessCreateUser(ctx, access.Principal{}, "v2-fixture-admin", "admin", hash, true, access.Meta{Peer: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	rest := httptest.NewServer(api.New(store, logger, api.WithAccess(access.New(store, access.Config{Mode: "required", SessionTTL: time.Hour})), api.WithAssets(store)))
	defer rest.Close()
	client := rest.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Post(rest.URL+"/api/auth/token", "application/json", strings.NewReader(`{"username":"v2-fixture-admin","password":"fixture-backend-long-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Token string `json:"access_token"`
	}
	err = json.NewDecoder(response.Body).Decode(&login)
	response.Body.Close()
	if err != nil || login.Token == "" {
		t.Fatal("REST login failed", err)
	}
	var assetID string
	// The fixture deliberately marks its BMC simulated. Keep that namespace;
	// reading unified inventory must not relabel a simulator as real hardware.
	if err = admin.QueryRow(ctx, "SELECT asset_id FROM "+quoted+".asset_source_bindings WHERE legacy_agent_id=$1 AND legacy_device_id=$2 ORDER BY inventory_at DESC,id LIMIT 1", agentID, deviceID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/devices/" + url.PathEscape(deviceID) + "/inventory?agent_id=" + url.QueryEscape(agentID), "/api/devices/" + url.PathEscape(deviceID) + "/telemetry?agent_id=" + url.QueryEscape(agentID), "/api/logs?agent_id=" + url.QueryEscape(agentID), "/api/assets/" + url.PathEscape(assetID) + "/resolved-inventory", "/api/assets/" + url.PathEscape(assetID) + "/placement", "/api/assets/" + url.PathEscape(assetID) + "/hardware?include_related=true", "/api/assets/" + url.PathEscape(assetID) + "/hardware/history?include_related=true", "/api/topology/global?scope=simulated"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", rest.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+login.Token)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("REST %s: %d %s %v", path, res.StatusCode, b, err)
		}
		if strings.Contains(path, "inventory") && (!strings.Contains(string(b), `"hardware"`) || !strings.Contains(string(b), `"Off"`)) {
			t.Fatal("REST hardware lost")
		}
		if strings.Contains(path, "/hardware") {
			var page struct {
				Items []struct {
					Kind, PowerState, Health string
					SourceRef                json.RawMessage `json:"source_ref"`
					Power                    string          `json:"power_state"`
				} `json:"items"`
			}
			if err = json.Unmarshal(b, &page); err != nil || len(page.Items) == 0 {
				t.Fatalf("Collector hardware Asset projection missing: %s %v", b, err)
			}
			off := false
			for _, item := range page.Items {
				if item.Power == "Off" {
					off = true
					if strings.EqualFold(item.Health, "critical") {
						t.Fatal("Off manufactured Critical health")
					}
				}
			}
			if !off {
				t.Fatalf("Asset hardware lost System power state: %s", b)
			}
		}
		if strings.Contains(path, "/topology/global") {
			var graph struct {
				Edges     []json.RawMessage               `json:"edges"`
				Relations []struct{ Type, Status string } `json:"management_relations"`
			}
			if err = json.Unmarshal(b, &graph); err != nil || len(graph.Edges) != 0 || len(graph.Relations) != 1 || graph.Relations[0].Type != "BMC_MANAGES_SYSTEM" || graph.Relations[0].Status != "resolved" {
				t.Fatalf("Collector BMC management relation missing or became cable: %s %v", b, err)
			}
		}
		if strings.Contains(path, "telemetry") && !strings.Contains(string(b), `"hardware_health"`) {
			t.Fatal("REST hardware telemetry lost")
		}
		if strings.Contains(path, "logs") && (!strings.Contains(string(b), `"syslog"`) || strings.Contains(string(b), "fixture-log-secret")) {
			t.Fatal("REST logs metadata/redaction lost")
		}
	}
	t.Logf("actual Collector v2: %d log delivery attempts, two unique records (including offline admission); Redfish inventory/metrics and authenticated REST verified", repo.logAttempts.Load())
}
