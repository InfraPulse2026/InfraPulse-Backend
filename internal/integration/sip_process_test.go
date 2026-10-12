package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
)

// Opt-in real Agent -> UDP SIP fixture -> controller -> PostgreSQL test.
// Agent source is not changed; provide a separately built executable.
func TestUnmodifiedAgentSIP401TimeoutAndRecovery(t *testing.T) {
	runAgentSIPScenario(t, false)
}
func TestUnmodifiedAgentSIPSustainedIncidentAndRecovery(t *testing.T) {
	runAgentSIPScenario(t, true)
}
func runAgentSIPScenario(t *testing.T, withIncidents bool) {
	binary, dbURL := os.Getenv("AGENT_BINARY"), os.Getenv("TEST_DATABASE_URL")
	if binary == "" || dbURL == "" {
		t.Skip("set AGENT_BINARY and TEST_DATABASE_URL for real Agent SIP integration")
	}
	dbConfig, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range append([]string{dbConfig.Host}, func() []string {
		hosts := []string{}
		for _, fallback := range dbConfig.Fallbacks {
			hosts = append(hosts, fallback.Host)
		}
		return hosts
	}()...) {
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			t.Fatal("SIP process fixture requires loopback PostgreSQL")
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("infrapulse_sip_process_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	expectedInterval := 30
	if withIncidents {
		expectedInterval = 3
	}
	store, err := storage.Open(ctx, parsed.String(), 10*time.Second, monitoring.Defaults(), expectedInterval)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterAgentServiceServer(g, controller.New(store, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go g.Serve(listen)
	defer g.Stop()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	var response atomic.Int32
	response.Store(401)
	go func() {
		buffer := make([]byte, 32768)
		for {
			n, remote, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			code := response.Load()
			if code == 0 {
				continue
			}
			headers := map[string]string{}
			for _, line := range strings.Split(string(buffer[:n]), "\r\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok {
					headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
				}
			}
			text := fmt.Sprintf("SIP/2.0 %d Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s;tag=fixture\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", code, headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"])
			_, _ = udp.WriteToUDP([]byte(text), remote)
		}
	}()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	cfg := fmt.Sprintf("agent:\n  name: sip-integration\n  state_dir: state\ncontroller:\n  address: %s\nsecurity:\n  mode: development\nlocal:\n  enabled: false\ncommands:\n  enabled: false\nqueue:\n  mode: durable\n  ack_fallback: reject\ntelephony:\n  endpoints:\n    - id: fixture-pbx\n      address: 127.0.0.1\n      port: %d\n      transport: udp\n      interval: 500ms\n      timeout: 100ms\n      failure_threshold: 2\n      latency_threshold: 250ms\ndevices: []\n", listen.Addr(), udp.LocalAddr().(*net.UDPAddr).Port)
	if err = os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "agent-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	command := exec.CommandContext(ctx, binary, "run", "--config", cfgPath)
	command.Dir = dir
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(strings.ToUpper(key), "INFRAPULSE_") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Stdout, command.Stderr = output, output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not exit")
		}
	}()
	wait := func(wanted string) telephony.Target {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			items, err := store.TelephonyTargets(ctx, telephony.Filter{TargetID: "fixture-pbx", Limit: 1})
			if err == nil && len(items) == 1 && items[0].Health.Status == wanted {
				return items[0]
			}
			select {
			case err := <-done:
				t.Fatalf("Agent exited: %v; log at %s", err, output.Name())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatalf("SIP state %s not observed; log at %s", wanted, output.Name())
		return telephony.Target{}
	}
	challenge := wait("degraded")
	if challenge.Latest.ResponseCode == nil || *challenge.Latest.ResponseCode != 401 || !challenge.Latest.Reachable || challenge.Latest.ErrorCategory != "authentication_required" {
		t.Fatalf("401 incorrectly treated as unreachable %+v", challenge)
	}
	response.Store(200)
	up := wait("up")
	if up.Latest.LatencyMS == nil || up.Latest.Source != "sip.options" {
		t.Fatal("real OPTIONS probe did not reach storage")
	}
	response.Store(0)
	down := wait("down")
	if down.Latest.Reachable || down.Latest.ResponseCode != nil || down.Latest.LatencyMS != nil || down.Latest.ConsecutiveFailures < 2 {
		t.Fatal("timeout fields or threshold lost")
	}
	waitIncident := func(state string) health.Incident {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			items, err := store.Incidents(ctx, health.Filter{AgentID: down.AgentID, ResourceKind: "sip", ResourceID: "fixture-pbx", RuleID: "sip.availability", State: state, Limit: 10})
			if err == nil && len(items) == 1 && (state != "open" || items[0].Severity == "critical") {
				return items[0]
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("SIP incident did not reach %s", state)
		return health.Incident{}
	}
	var incident health.Incident
	if withIncidents {
		incident = waitIncident("open")
		if incident.Severity != "critical" || incident.DeviceID != "" || len(incident.Signal.ResourceEvidence) < 3 || incident.Signal.WindowSeconds < 6 {
			t.Fatal("real Agent timeout incident has insufficient/wrong evidence")
		}
	}
	response.Store(200)
	recovered := wait("up")
	if recovered.Latest.ConsecutiveFailures != 0 {
		t.Fatal("recovery did not reset failures")
	}
	if withIncidents {
		resolved := waitIncident("resolved")
		if resolved.ID != incident.ID {
			t.Fatal("recovery changed incident identity")
		}
		evidence, err := store.IncidentEvidence(ctx, resolved.ID)
		if err != nil || len(evidence) < 2 || evidence[0].Kind != "recovered" {
			t.Fatal("real Agent incident/recovery evidence missing")
		}
		t.Log("unmodified Agent UDP timeout produced one sustained SIP incident; healthy OPTIONS stably resolved it")
	}
	history, err := store.TelephonyHistory(ctx, telephony.HistoryFilter{AgentID: recovered.AgentID, TargetID: recovered.TargetID, Limit: 100})
	if err != nil || len(history.Items) < 4 {
		t.Fatalf("real probe history missing %+v %v", history, err)
	}
}
