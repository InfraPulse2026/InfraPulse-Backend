// Independent Linux control node. It never starts HTTP/gRPC or migrates/reset
// Agent connections. Start the backend first to initialize schema 033.
package main

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/safelog"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	log := slog.New(safelog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("automation worker stopped", "reason", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, log *slog.Logger) error {
	if runtime.GOOS != "linux" {
		return errors.New("run Automation Worker on a Linux control node")
	}
	id := os.Getenv("AUTOMATION_WORKER_ID")
	if !automation.Identifier(id) {
		return errors.New("AUTOMATION_WORKER_ID must be 1..64 ASCII letters, numbers, underscore or hyphen")
	}
	vault, err := datasources.NewVault(os.Getenv("AUTOMATION_ENCRYPTION_KEY"))
	if err != nil || vault == nil {
		return errors.New("AUTOMATION_ENCRYPTION_KEY must encode 32 bytes")
	}
	executor := automation.Process{Python: os.Getenv("AUTOMATION_PYTHON"), CredentialsDir: os.Getenv("AUTOMATION_CREDENTIALS_DIR"), WorkDir: os.Getenv("AUTOMATION_WORK_DIR")}
	for _, path := range []string{executor.Python, executor.CredentialsDir, executor.WorkDir} {
		if !filepath.IsAbs(path) {
			return errors.New("automation runtime paths must be absolute")
		}
	}
	if err = os.MkdirAll(executor.WorkDir, 0700); err != nil {
		return err
	}
	for _, item := range strings.Split(os.Getenv("AUTOMATION_ALLOWED_CIDRS"), ",") {
		prefix, e := netip.ParsePrefix(strings.TrimSpace(item))
		if e != nil || prefix.Addr().Is4In6() || prefix.Bits() == 0 {
			return errors.New("AUTOMATION_ALLOWED_CIDRS requires explicit narrow network prefixes")
		}
		executor.AllowedNetworks = append(executor.AllowedNetworks, prefix.Masked())
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	store, err := storage.Open(startup, os.Getenv("DATABASE_URL"), 90*time.Second, monitoring.Defaults())
	if err != nil {
		return errors.New("cannot connect Automation Worker to PostgreSQL")
	}
	defer store.Close()
	if err = store.CheckAutomationSchema(startup); err != nil {
		return err
	}
	if err = store.ConfigureAutomation(startup, true, vault); err != nil {
		return err
	}
	return automation.Run(ctx, store, executor, id, log)
}
