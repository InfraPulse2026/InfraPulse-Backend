package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestServerStartupReadinessAndShutdown(t *testing.T) {
	database := os.Getenv("TEST_DATABASE_URL")
	if database == "" {
		t.Skip("set TEST_DATABASE_URL for server lifecycle integration test")
	}
	admin, err := pgx.Connect(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("infrapulse_lifecycle_%d", time.Now().UnixNano())
	if _, err = admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	u, err := url.Parse(database)
	if err != nil || u.Scheme == "" {
		t.Fatal("lifecycle test expects a PostgreSQL URL")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	httpAddress := freeAddress(t)
	grpcAddress := freeAddress(t)
	t.Setenv("DATABASE_URL", u.String())
	t.Setenv("HTTP_ADDRESS", httpAddress)
	t.Setenv("GRPC_ADDRESS", grpcAddress)
	for _, key := range []string{"TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE", "AGENT_TIMEOUT", "DEFAULT_TELEMETRY_INTERVAL", "DEFAULT_INVENTORY_INTERVAL", "DEFAULT_TOPOLOGY_INTERVAL", "AUTH_MODE", "AUTH_SESSION_TTL", "AUTH_PUBLIC_ORIGIN", "AUTH_COOKIE_SECURE"} {
		t.Setenv(key, "")
	}
	// Start a second runtime against the same schema and listener addresses.
	// This checks repeatable migrations, worker shutdown and port release.
	for generation := 1; generation <= 2; generation++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
		client := &http.Client{Timeout: 200 * time.Millisecond}
		ready := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get("http://" + httpAddress + "/readyz")
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == 200 {
					ready = true
					break
				}
			}
			select {
			case err := <-result:
				t.Fatalf("server exited before readiness: %v", err)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ready {
			t.Fatal("server never became ready")
		}
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown timed out")
		}
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}
