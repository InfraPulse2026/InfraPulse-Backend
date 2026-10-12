package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	cc "github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"sync"
)

func TestUnmodifiedCollectorManagedConfigLiveApplyRejectRestartAndClear(t *testing.T) {
	binary, db := os.Getenv("COLLECTOR_V2_BINARY"), os.Getenv("TEST_DATABASE_URL")
	if binary == "" || db == "" {
		t.Skip("set AGENT_BINARY and TEST_DATABASE_URL for real Agent PKI process test")
	}
	parsed, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}
	dbConfig, err := pgx.ParseConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range append([]string{parsed.Hostname(), dbConfig.Host}, func() []string {
		hosts := []string{}
		for _, fallback := range dbConfig.Fallbacks {
			hosts = append(hosts, fallback.Host)
		}
		return hosts
	}()...) {
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			t.Fatal("PKI process fixture requires loopback PostgreSQL")
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("infrapulse_pki_process_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("clean up PKI schema: %v", err)
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
	const id = "bf6e7560-9b04-4d28-ae98-5dfe7d4f2001"
	dir := t.TempDir()
	caDir := filepath.Join(dir, "credentials")
	if err = pki.InitCA(caDir, []string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := os.ReadFile(filepath.Join(caDir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := pki.NewIssuer(caPEM, caKey, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(caDir, "server.pem"), filepath.Join(caDir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	service := &pki.Service{Repository: store, Issuer: issuer}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var activeFingerprint atomic.Value
	activeFingerprint.Store("")
	store.ConfigureCollectorConfig(true)
	control := controller.New(store, logger, controller.WithManagedConfiguration(true), controller.WithAgentAuthorizer(func(ctx context.Context, id string) error {
		if err := service.AuthorizePeer(ctx, id); err != nil {
			return err
		}
		remote, ok := peer.FromContext(ctx)
		if !ok {
			return pki.ErrDenied
		}
		info, ok := remote.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return pki.ErrDenied
		}
		activeFingerprint.Store(pki.Fingerprint(info.State.VerifiedChains[0][0]))
		return nil
	}))
	workerCtx, workerStop := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); cc.Run(workerCtx, store, control, logger) }()
	defer func() { workerStop(); <-workerDone }()
	service.OnRevoke = control.DisconnectAgent
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})))
	pb.RegisterAgentServiceServer(g, control)
	go func() { _ = g.Serve(listener) }()
	defer g.Stop()
	enrollment := httptest.NewUnstartedServer(service.Handler())
	enrollment.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.VerifyClientCertIfGiven}
	enrollment.StartTLS()
	defer enrollment.Close()
	const password = "pki-process-fixture-admin-password"
	passwordHash, err := access.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AccessCreateUser(ctx, access.Principal{}, "pki-owner", "admin", passwordHash, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	auth := access.New(store, access.Config{Mode: "required", SessionTTL: time.Hour})
	principal, err := auth.Login(ctx, "pki-owner", password, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	rest := httptest.NewServer(api.New(store, logger, api.WithAccess(auth), api.WithPKI(service), api.WithCollectorConfig(store)))
	defer rest.Close()
	var requestKey atomic.Uint32
	request := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			reader = bytes.NewReader(encoded)
		}
		req, e := http.NewRequestWithContext(ctx, method, rest.URL+path, reader)
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+principal.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("managed-process-%d", requestKey.Add(1)))
		response, e := rest.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		data, e := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if e != nil {
			t.Fatal(e)
		}
		return response.StatusCode, data
	}
	code, data := request("POST", "/api/pki/bootstrap-tokens", map[string]any{"agent_id": id, "ttl_seconds": 300})
	var bootstrap pki.Token
	if code != 201 || json.Unmarshal(data, &bootstrap) != nil || bootstrap.Secret == "" {
		t.Fatalf("bootstrap API status %d", code)
	}
	state := filepath.Join(dir, "state")
	if err = os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state, "agent-id"), []byte(id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	var managedCount, localCount atomic.Uint32
	var probesMu sync.Mutex
	var probes []time.Time
	go func() {
		buffer := make([]byte, 32768)
		for {
			n, remote, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			headers := map[string]string{}
			for _, line := range strings.Split(string(buffer[:n]), "\r\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok {
					headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
				}
			}
			if strings.Contains(headers["from"], "managed-test") {
				managedCount.Add(1)
				probesMu.Lock()
				probes = append(probes, time.Now())
				probesMu.Unlock()
			} else {
				localCount.Add(1)
			}
			response := fmt.Sprintf("SIP/2.0 200 Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s;tag=fixture\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"])
			_, _ = udp.WriteToUDP([]byte(response), remote)
		}
	}()
	config := fmt.Sprintf(`agent:
  name: pki-process-fixture
  state_dir: state
controller:
  address: %s
  connect_timeout: 2s
  reconnect_initial: 100ms
  reconnect_max: 1s
security:
  mode: production
  server_name: localhost
  renew_before: 168h
  enrollment:
    enabled: true
    url: %s
    ca_file: credentials/ca.pem
    server_name: localhost
    token_env: PKI_PROCESS_BOOTSTRAP_TOKEN
    timeout: 5s
local:
  enabled: false
queue:
  mode: durable
  ack_fallback: reject
commands:
  enabled: false
telemetry:
  fast_interval: 3s
  medium_interval: 3s
  inventory_interval: 3s
  topology_interval: 3s
  heartbeat_interval: 1s
`, listener.Addr(), enrollment.URL)
	config += fmt.Sprintf(`remote_management:
  enabled: true
  allowed_protocols: [sip, snmp]
  allowed_destinations: [127.0.0.0/8]
telephony:
  endpoints:
    - id: local-pbx
      address: 127.0.0.1
      port: %d
      transport: udp
      from_user: local-test
      interval: 1s
      timeout: 500ms
`, udp.LocalAddr().(*net.UDPAddr).Port)
	configPath := filepath.Join(dir, "agent.yaml")
	if err = os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	start := func(withToken bool) (*exec.Cmd, chan error) {
		t.Helper()
		process := exec.CommandContext(ctx, binary, "run", "--config", configPath)
		process.Dir = dir
		process.Stdout = io.Discard
		process.Stderr = io.Discard
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			key = strings.ToUpper(key)
			if strings.HasPrefix(key, "INFRAPULSE_") || key == "PKI_PROCESS_BOOTSTRAP_TOKEN" {
				continue
			}
			process.Env = append(process.Env, value)
		}
		if withToken {
			process.Env = append(process.Env, "PKI_PROCESS_BOOTSTRAP_TOKEN="+bootstrap.Secret)
		}
		if e := process.Start(); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { done <- process.Wait() }()
		return process, done
	}
	process, done := start(true)
	running := true
	stop := func() {
		if running {
			_ = process.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("PKI Agent did not stop")
			}
			running = false
		}
	}
	defer func() { stop() }()
	wait := func(description string, timeout time.Duration, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case err := <-done:
				running = false
				t.Fatalf("Agent exited while waiting for %s: %v", description, err)
			default:
			}
			if predicate() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s", description)
	}
	wait("managed mTLS Collector opt-in", 15*time.Second, func() bool {
		state, err := store.CollectorConfig(ctx, id)
		return err == nil && state.Connected && state.ManagedConfigV1 && state.RemoteManagementEnabled && control.HasManagedSession(id, state.SessionID)
	})
	initial, _ := store.CollectorConfig(ctx, id)
	firstSession := initial.SessionID
	pid := process.Process.Pid
	put := func(expected int64, targets any, allowEmpty bool) cc.Revision {
		t.Helper()
		code, body := request("PUT", "/api/collectors/"+id+"/config", map[string]any{"expected_revision": expected, "confirmed": true, "reason": "managed Collector integration fixture", "targets": targets, "allow_empty_snapshot": allowEmpty, "site": "demo-site"})
		var rev cc.Revision
		if code != 202 || json.Unmarshal(body, &rev) != nil {
			t.Fatalf("config API %d: %s", code, body)
		}
		return rev
	}
	target := func(interval string) map[string]any {
		return map[string]any{"target_id": "managed-pbx", "protocol": "sip", "endpoint": "127.0.0.1", "enabled": true, "sip": map[string]any{"port": udp.LocalAddr().(*net.UDPAddr).Port, "from_user": "managed-test"}, "polling": map[string]string{"fast_interval": interval, "timeout": "0.500s"}}
	}
	applied := func(revision int64) bool {
		state, err := store.CollectorConfig(ctx, id)
		return err == nil && state.InSync && state.AppliedRevision == revision
	}
	first := put(0, []any{target("3s")}, false)
	wait("full snapshot applied and real SIP probe", 15*time.Second, func() bool { return applied(first.Revision) && managedCount.Load() > 0 && localCount.Load() > 0 })
	second := put(first.Revision, []any{target("6s")}, false)
	wait("interval changed without restart", 15*time.Second, func() bool { return applied(second.Revision) })
	probesMu.Lock()
	baseline := len(probes)
	probesMu.Unlock()
	wait("two actual probes at new interval", 20*time.Second, func() bool { probesMu.Lock(); defer probesMu.Unlock(); return len(probes) >= baseline+2 })
	probesMu.Lock()
	gap := probes[len(probes)-1].Sub(probes[len(probes)-2])
	probesMu.Unlock()
	targets, targetErr := store.TelephonyTargets(ctx, telephony.Filter{AgentID: id, Limit: 50})
	inherited := false
	for _, item := range targets {
		if item.TargetID == "managed-pbx" {
			inherited = item.Monitoring.Origin == "collector_applied" && item.Monitoring.ExpectedIntervalSeconds == 6
		}
	}
	if targetErr != nil || !inherited {
		t.Fatalf("real managed SIP polling not reflected in backend freshness: %+v %v", targets, targetErr)
	}
	if gap < 5500*time.Millisecond || gap > 8500*time.Millisecond {
		t.Fatal("actual scheduler interval", gap)
	}
	liveState, _ := store.CollectorConfig(ctx, id)
	if liveState.SessionID != firstSession || process.Process.Pid != pid {
		t.Fatal("configuration restarted Collector")
	}
	rejected := put(second.Revision, []any{target("6s"), map[string]any{"target_id": "denied", "protocol": "snmp", "endpoint": "127.0.0.1", "enabled": true, "credential_ref": "missing-local-credential"}}, false)
	wait("Collector rejected unauthorized credential reference", 15*time.Second, func() bool {
		s, e := store.CollectorConfig(ctx, id)
		return e == nil && s.Status == "rejected" && s.StatusRevision == rejected.Revision && s.AppliedRevision == second.Revision && !s.InSync
	})
	previousCount := managedCount.Load()
	wait("last good runtime still probes after rejection", 10*time.Second, func() bool { return managedCount.Load() > previousCount })
	stop()
	process, done = start(false)
	running = true
	wait("LKG restored on token-free restart", 15*time.Second, func() bool {
		s, e := store.CollectorConfig(ctx, id)
		return e == nil && s.Connected && s.SessionID != firstSession && s.AppliedRevision == second.Revision
	})
	cleared := put(rejected.Revision, []any{}, true)
	wait("empty managed set applied", 15*time.Second, func() bool { return applied(cleared.Revision) })
	managedBefore, localBefore := managedCount.Load(), localCount.Load()
	wait("local YAML endpoint preserved after managed clear", 5*time.Second, func() bool { return localCount.Load() >= localBefore+2 })
	if managedCount.Load() != managedBefore {
		t.Fatal("cleared managed endpoint still probes")
	}
	code, body := request("GET", "/api/collectors/"+id+"/config/events", nil)
	if code != 200 || !bytes.Contains(body, []byte("applied")) || !bytes.Contains(body, []byte("rejected")) {
		t.Fatal("REST configuration lifecycle", code)
	}
	t.Log("unmodified final Collector: mTLS, live 3s→6s scheduler, rejection/LKG, restart and local endpoint preservation passed")
}
