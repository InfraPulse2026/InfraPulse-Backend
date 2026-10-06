package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/config"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	store, err := storage.Open(startup, cfg.DatabaseURL, cfg.AgentTimeout, cfg.MonitoringDefaults)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer store.Close()
	if err = store.Migrate(startup); err != nil {
		return fmt.Errorf("migrate PostgreSQL: %w", err)
	}
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(4 << 20), grpc.MaxSendMsgSize(4 << 20)}
	if cfg.TLSCert != "" {
		tlsConfig, err := serverTLS(cfg)
		if err != nil {
			return err
		}
		options = append(options, grpc.Creds(credentials.NewTLS(tlsConfig)))
	} else {
		log.Warn("development gRPC without TLS; use local published ports only")
	}
	g := grpc.NewServer(options...)
	pb.RegisterAgentServiceServer(g, controller.New(store, log))
	grpcListener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		return err
	}
	defer grpcListener.Close()
	httpListener, err := net.Listen("tcp", cfg.HTTPAddress)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	h := &http.Server{Handler: api.New(store, log), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	results := make(chan error, 2)
	go func() { results <- g.Serve(grpcListener) }()
	go func() { results <- h.Serve(httpListener) }()
	log.Info("backend ready", "http", cfg.HTTPAddress, "grpc", cfg.GRPCAddress)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-results:
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	grpcDone := make(chan struct{})
	go func() { g.GracefulStop(); close(grpcDone) }()
	httpErr := h.Shutdown(shutdown)
	if httpErr != nil {
		_ = h.Close()
	}
	select {
	case <-grpcDone:
	case <-shutdown.Done():
		g.Stop()
		<-grpcDone
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return serveErr
	}
	log.Info("backend shutdown complete")
	return nil
}

func serverTLS(cfg config.Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS credentials: %w", err)
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if cfg.TLSClientCA != "" {
		b, err := os.ReadFile(cfg.TLSClientCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("client CA file has no certificates")
		}
		c.ClientCAs = pool
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, nil
}
