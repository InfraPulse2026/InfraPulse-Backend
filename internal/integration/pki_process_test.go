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
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestUnmodifiedAgentEnrollAutomaticRenewRestartAndRevoke(t *testing.T) {
	binary, db := os.Getenv("AGENT_BINARY"), os.Getenv("TEST_DATABASE_URL")
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
	ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
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
	control := controller.New(store, logger, controller.WithAgentAuthorizer(func(ctx context.Context, id string) error {
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
	rest := httptest.NewServer(api.New(store, logger, api.WithAccess(auth), api.WithPKI(service)))
	defer rest.Close()
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
	wait("enrollment and authenticated heartbeat", 15*time.Second, func() bool {
		agents, e := store.Agents(ctx)
		if e != nil || len(agents) != 1 || !agents[0].Connected || len(agents[0].Heartbeat) == 0 {
			return false
		}
		return true
	})
	certs, err := store.PKICertificates(ctx, id, 100)
	if err != nil || len(certs) != 1 {
		t.Fatal("Agent did not enroll exactly once")
	}
	first := certs[0].Fingerprint
	tokens, err := store.PKITokens(ctx, id, 100)
	if err != nil || len(tokens) != 1 || tokens[0].UsedAt == nil || tokens[0].Secret != "" {
		t.Fatal("bootstrap not consumed")
	}
	t.Log("Agent enrolled via HTTPS, certificate bound to UUID; waiting for its built-in one-minute renewal tick")
	var renewed string
	wait("automatic certificate rotation and gRPC reconnect", 75*time.Second, func() bool {
		certs, e := store.PKICertificates(ctx, id, 100)
		if e != nil || len(certs) != 2 {
			return false
		}
		for _, cert := range certs {
			if cert.RenewedFrom != nil && *cert.RenewedFrom == first {
				renewed = cert.Fingerprint
			}
		}
		agents, e := store.Agents(ctx)
		return renewed != "" && e == nil && len(agents) == 1 && agents[0].Connected && activeFingerprint.Load().(string) == renewed
	})
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rotatedSession := agents[0].SessionID
	stop()
	// Restart without any bootstrap secret: durable Agent bundle must suffice.
	process, done = start(false)
	running = true
	wait("restart using stored certificate without token", 15*time.Second, func() bool {
		agents, e := store.Agents(ctx)
		return e == nil && len(agents) == 1 && agents[0].Connected && agents[0].SessionID != rotatedSession
	})
	certs, err = store.PKICertificates(ctx, id, 100)
	if err != nil || len(certs) != 2 {
		t.Fatal("restart unexpectedly reenrolled")
	}
	code, _ = request("POST", "/api/pki/certificates/"+renewed+"/revoke", map[string]bool{"confirmed": true})
	if code != 200 {
		t.Fatalf("revoke API status %d", code)
	}
	wait("revocation closes stream", 10*time.Second, func() bool {
		agents, e := store.Agents(ctx)
		return e == nil && len(agents) == 1 && !agents[0].Connected
	})
	time.Sleep(1200 * time.Millisecond)
	agents, err = store.Agents(ctx)
	if err != nil || agents[0].Connected {
		t.Fatal("revoked certificate reconnected")
	}
	if err = store.PKICertificateActive(ctx, id, renewed); err != pki.ErrDenied {
		t.Fatal("revoked certificate remains authorized")
	}
	t.Log("automatic renewal, token-free restart and administrative revocation passed with unmodified Agent")
}
